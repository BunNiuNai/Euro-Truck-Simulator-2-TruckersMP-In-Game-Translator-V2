// 极简 JSON 实现。设计约束见 json.hpp 顶部说明。
#include "json.hpp"

#include <cmath>
#include <cstdio>
#include <cstdlib>
#include <cstring>

namespace tn {

namespace {

constexpr int kMaxDepth = 64;

class Parser {
public:
    Parser(const std::string& text) : s_(text), i_(0) {}

    bool parseValue(Json& out, int depth) {
        if (depth > kMaxDepth) { fail("嵌套过深"); return false; }
        skipWs();
        if (i_ >= s_.size()) { fail("内容意外结束"); return false; }

        const char c = s_[i_];
        switch (c) {
            case '{': return parseObject(out, depth);
            case '[': return parseArray(out, depth);
            case '"': {
                std::string v;
                if (!parseString(v)) return false;
                out = Json(v);
                return true;
            }
            case 't': return parseLiteral("true", Json(true), out);
            case 'f': return parseLiteral("false", Json(false), out);
            case 'n': return parseLiteral("null", Json(), out);
            default:
                if (c == '-' || (c >= '0' && c <= '9')) return parseNumber(out);
                fail("无法识别的值起始字符");
                return false;
        }
    }

    bool atEndAfterWs() {
        skipWs();
        return i_ >= s_.size();
    }

    const std::string& error() const { return err_; }

private:
    void skipWs() {
        while (i_ < s_.size()) {
            const char c = s_[i_];
            if (c == ' ' || c == '\t' || c == '\n' || c == '\r') { ++i_; continue; }
            break;
        }
    }

    void fail(const char* what) {
        if (!err_.empty()) return;  // 保留第一个错误
        char buf[128];
        std::snprintf(buf, sizeof(buf), "位置 %zu: %s", i_, what);
        err_ = buf;
    }

    bool parseLiteral(const char* lit, Json value, Json& out) {
        const size_t n = std::strlen(lit);
        if (s_.compare(i_, n, lit) != 0) { fail("字面量不匹配"); return false; }
        i_ += n;
        out = std::move(value);
        return true;
    }

    // JSON 数字文法： [ - ] int [ frac ] [ exp ]
    //   int  = 0 / ( digit1-9 *DIGIT )      ← 不允许前导零
    //   frac = "." 1*DIGIT                   ← 小数点后必须有数字
    //   exp  = ("e"/"E") ["+"/"-"] 1*DIGIT   ← 指数后必须有数字
    bool parseNumber(Json& out) {
        const size_t start = i_;
        if (i_ < s_.size() && s_[i_] == '-') ++i_;

        if (i_ >= s_.size() || s_[i_] < '0' || s_[i_] > '9') {
            fail("数字缺少整数部分");
            return false;
        }
        if (s_[i_] == '0') {
            ++i_;
            if (i_ < s_.size() && s_[i_] >= '0' && s_[i_] <= '9') {
                fail("数字不允许前导零");
                return false;
            }
        } else {
            while (i_ < s_.size() && s_[i_] >= '0' && s_[i_] <= '9') ++i_;
        }

        if (i_ < s_.size() && s_[i_] == '.') {
            ++i_;
            if (i_ >= s_.size() || s_[i_] < '0' || s_[i_] > '9') {
                fail("小数点后缺少数字");
                return false;
            }
            while (i_ < s_.size() && s_[i_] >= '0' && s_[i_] <= '9') ++i_;
        }

        if (i_ < s_.size() && (s_[i_] == 'e' || s_[i_] == 'E')) {
            ++i_;
            if (i_ < s_.size() && (s_[i_] == '+' || s_[i_] == '-')) ++i_;
            if (i_ >= s_.size() || s_[i_] < '0' || s_[i_] > '9') {
                fail("指数部分缺少数字");
                return false;
            }
            while (i_ < s_.size() && s_[i_] >= '0' && s_[i_] <= '9') ++i_;
        }

        const std::string slice = s_.substr(start, i_ - start);
        char* end = nullptr;
        const double v = std::strtod(slice.c_str(), &end);
        if (end == nullptr || *end != '\0') { fail("数字格式非法"); return false; }
        if (!std::isfinite(v)) { fail("数字超出范围"); return false; }
        out = Json(v);
        return true;
    }

    bool parseString(std::string& out) {
        if (i_ >= s_.size() || s_[i_] != '"') { fail("字符串缺少引号"); return false; }
        ++i_;
        out.clear();

        while (true) {
            if (i_ >= s_.size()) { fail("字符串未闭合"); return false; }
            const unsigned char c = static_cast<unsigned char>(s_[i_]);

            if (c == '"') { ++i_; return true; }

            if (c == '\\') {
                ++i_;
                if (i_ >= s_.size()) { fail("转义序列未完成"); return false; }
                const char e = s_[i_++];
                switch (e) {
                    case '"':  out.push_back('"');  break;
                    case '\\': out.push_back('\\'); break;
                    case '/':  out.push_back('/');  break;
                    case 'b':  out.push_back('\b'); break;
                    case 'f':  out.push_back('\f'); break;
                    case 'n':  out.push_back('\n'); break;
                    case 'r':  out.push_back('\r'); break;
                    case 't':  out.push_back('\t'); break;
                    case 'u': {
                        unsigned int cp = 0;
                        if (!parseHex4(cp)) return false;
                        // 代理对：\uD800-\uDBFF 后必须跟 \uDC00-\uDFFF
                        if (cp >= 0xD800 && cp <= 0xDBFF) {
                            if (i_ + 1 < s_.size() && s_[i_] == '\\' && s_[i_ + 1] == 'u') {
                                i_ += 2;
                                unsigned int low = 0;
                                if (!parseHex4(low)) return false;
                                if (low >= 0xDC00 && low <= 0xDFFF) {
                                    cp = 0x10000 + ((cp - 0xD800) << 10) + (low - 0xDC00);
                                } else {
                                    appendUtf8(out, 0xFFFD);  // 落单的低代理 → 替换字符
                                    cp = low;
                                }
                            } else {
                                appendUtf8(out, 0xFFFD);
                                continue;
                            }
                        }
                        appendUtf8(out, cp);
                        break;
                    }
                    default:
                        fail("未知转义字符");
                        return false;
                }
                continue;
            }

            // 控制字符必须转义（JSON 规范）
            if (c < 0x20) { fail("字符串里出现未转义的控制字符"); return false; }

            out.push_back(static_cast<char>(c));
            ++i_;
        }
    }

    bool parseHex4(unsigned int& out) {
        if (i_ + 4 > s_.size()) { fail("\\u 转义不足 4 位"); return false; }
        unsigned int v = 0;
        for (int k = 0; k < 4; ++k) {
            const char c = s_[i_ + k];
            v <<= 4;
            if (c >= '0' && c <= '9')      v |= static_cast<unsigned>(c - '0');
            else if (c >= 'a' && c <= 'f') v |= static_cast<unsigned>(c - 'a' + 10);
            else if (c >= 'A' && c <= 'F') v |= static_cast<unsigned>(c - 'A' + 10);
            else { fail("\\u 转义含非法十六进制字符"); return false; }
        }
        i_ += 4;
        out = v;
        return true;
    }

    bool parseArray(Json& out, int depth) {
        ++i_;  // 消费 '['
        out = Json::makeArray();
        skipWs();
        if (i_ < s_.size() && s_[i_] == ']') { ++i_; return true; }

        while (true) {
            Json item;
            if (!parseValue(item, depth + 1)) return false;
            out.array.push_back(std::move(item));
            skipWs();
            if (i_ >= s_.size()) { fail("数组未闭合"); return false; }
            if (s_[i_] == ',') { ++i_; skipWs(); continue; }
            if (s_[i_] == ']') { ++i_; return true; }
            fail("数组里期望 ',' 或 ']'");
            return false;
        }
    }

    bool parseObject(Json& out, int depth) {
        ++i_;  // 消费 '{'
        out = Json::makeObject();
        skipWs();
        if (i_ < s_.size() && s_[i_] == '}') { ++i_; return true; }

        while (true) {
            skipWs();
            std::string key;
            if (!parseString(key)) return false;
            skipWs();
            if (i_ >= s_.size() || s_[i_] != ':') { fail("对象里期望 ':'"); return false; }
            ++i_;
            Json value;
            if (!parseValue(value, depth + 1)) return false;
            out.object.emplace_back(std::move(key), std::move(value));

            skipWs();
            if (i_ >= s_.size()) { fail("对象未闭合"); return false; }
            if (s_[i_] == ',') { ++i_; continue; }
            if (s_[i_] == '}') { ++i_; return true; }
            fail("对象里期望 ',' 或 '}'");
            return false;
        }
    }

    const std::string& s_;
    size_t i_;
    std::string err_;
};

void dumpString(std::string& out, const std::string& s) {
    out.push_back('"');
    for (unsigned char c : s) {
        switch (c) {
            case '"':  out += "\\\""; break;
            case '\\': out += "\\\\"; break;
            case '\b': out += "\\b";  break;
            case '\f': out += "\\f";  break;
            case '\n': out += "\\n";  break;
            case '\r': out += "\\r";  break;
            case '\t': out += "\\t";  break;
            default:
                if (c < 0x20) {
                    char buf[8];
                    std::snprintf(buf, sizeof(buf), "\\u%04x", c);
                    out += buf;
                } else {
                    // UTF-8 字节原样输出（合法 UTF-8 无需转义）
                    out.push_back(static_cast<char>(c));
                }
        }
    }
    out.push_back('"');
}

void dumpNumber(std::string& out, double v) {
    if (v == static_cast<double>(static_cast<long long>(v))) {
        char buf[32];
        std::snprintf(buf, sizeof(buf), "%lld", static_cast<long long>(v));
        out += buf;
        return;
    }
    char buf[40];
    std::snprintf(buf, sizeof(buf), "%.17g", v);
    out += buf;
}

void dumpValue(std::string& out, const Json& j) {
    switch (j.type) {
        case JsonType::Null:   out += "null"; break;
        case JsonType::Bool:   out += j.boolValue ? "true" : "false"; break;
        case JsonType::Number: dumpNumber(out, j.numberValue); break;
        case JsonType::String: dumpString(out, j.stringValue); break;
        case JsonType::Array: {
            out.push_back('[');
            for (size_t k = 0; k < j.array.size(); ++k) {
                if (k) out.push_back(',');
                dumpValue(out, j.array[k]);
            }
            out.push_back(']');
            break;
        }
        case JsonType::Object: {
            out.push_back('{');
            for (size_t k = 0; k < j.object.size(); ++k) {
                if (k) out.push_back(',');
                dumpString(out, j.object[k].first);
                out.push_back(':');
                dumpValue(out, j.object[k].second);
            }
            out.push_back('}');
            break;
        }
    }
}

}  // namespace

const Json* Json::find(const std::string& key) const {
    if (type != JsonType::Object) return nullptr;
    for (const auto& kv : object) {
        if (kv.first == key) return &kv.second;  // 重复键取第一个
    }
    return nullptr;
}

bool Json::parse(const std::string& text, Json& out, std::string& err) {
    Parser p(text);
    Json value;
    if (!p.parseValue(value, 0)) { err = p.error(); return false; }
    if (!p.atEndAfterWs()) { err = "结尾有多余内容"; return false; }
    out = std::move(value);
    return true;
}

std::string Json::dump() const {
    std::string out;
    out.reserve(64);
    dumpValue(out, *this);
    return out;
}

void appendUtf8(std::string& out, unsigned int cp) {
    if (cp <= 0x7F) {
        out.push_back(static_cast<char>(cp));
    } else if (cp <= 0x7FF) {
        out.push_back(static_cast<char>(0xC0 | (cp >> 6)));
        out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
    } else if (cp <= 0xFFFF) {
        out.push_back(static_cast<char>(0xE0 | (cp >> 12)));
        out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
        out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
    } else {
        out.push_back(static_cast<char>(0xF0 | (cp >> 18)));
        out.push_back(static_cast<char>(0x80 | ((cp >> 12) & 0x3F)));
        out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
        out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
    }
}

}  // namespace tn
