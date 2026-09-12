// 极简 JSON DOM 与解析/序列化 —— Translator V2 native 层自用。
//
// 为什么要自己写（而不是引入 nlohmann/json 之类）：
//   1. ADR-005「C++ 组织方式约束」要求单元小而自证、依赖少；
//   2. 本机无法拉取第三方源码，引入不可验证的依赖风险更高；
//   3. 我们只需要一个很小的子集：解析 Go 发来的命令、序列化发给 Go 的事件。
//
// 支持范围（够用即可，明确不做）：
//   支持：null / true / false / 数字 / 字符串（含 \uXXXX 与代理对）/ 数组 / 对象
//   不做：注释、NaN/Infinity、重复键合并（保留多个，find 取第一个）、深度 > 64
//
// 安全约定：解析失败返回 false 并给出错误描述，**绝不抛异常、绝不越界读**。
#ifndef TN_JSON_HPP
#define TN_JSON_HPP

#include <cstddef>
#include <string>
#include <utility>
#include <vector>

namespace tn {

enum class JsonType { Null, Bool, Number, String, Array, Object };

class Json {
public:
    JsonType type = JsonType::Null;

    bool boolValue = false;
    double numberValue = 0.0;
    std::string stringValue;
    std::vector<Json> array;
    std::vector<std::pair<std::string, Json>> object;

    Json() = default;
    explicit Json(bool v) : type(JsonType::Bool), boolValue(v) {}
    explicit Json(double v) : type(JsonType::Number), numberValue(v) {}
    explicit Json(const std::string& v) : type(JsonType::String), stringValue(v) {}
    explicit Json(const char* v) : type(JsonType::String), stringValue(v) {}

    static Json makeArray() { Json j; j.type = JsonType::Array; return j; }
    static Json makeObject() { Json j; j.type = JsonType::Object; return j; }

    bool isNull() const { return type == JsonType::Null; }
    bool isString() const { return type == JsonType::String; }
    bool isNumber() const { return type == JsonType::Number; }
    bool isArray() const { return type == JsonType::Array; }
    bool isObject() const { return type == JsonType::Object; }

    // 取对象成员；不存在或自身不是对象时返回 nullptr
    const Json* find(const std::string& key) const;

    std::string asString(const std::string& def = std::string()) const {
        return type == JsonType::String ? stringValue : def;
    }
    double asNumber(double def = 0.0) const {
        return type == JsonType::Number ? numberValue : def;
    }
    bool asBool(bool def = false) const {
        return type == JsonType::Bool ? boolValue : def;
    }
    int asInt(int def = 0) const {
        return type == JsonType::Number ? static_cast<int>(numberValue) : def;
    }

    // 便捷构造（链式写入）
    void set(const std::string& key, Json value) {
        if (type != JsonType::Object) { type = JsonType::Object; object.clear(); }
        object.emplace_back(key, std::move(value));
    }
    void push(Json value) {
        if (type != JsonType::Array) { type = JsonType::Array; array.clear(); }
        array.push_back(std::move(value));
    }

    // 解析；失败时 err 给出位置与原因
    static bool parse(const std::string& text, Json& out, std::string& err);

    // 序列化（紧凑形式）
    std::string dump() const;
};

// 把 UTF-16 码元编码进 UTF-8（内部使用，暴露出来便于单测）
void appendUtf8(std::string& out, unsigned int codePoint);

}  // namespace tn

#endif  // TN_JSON_HPP
