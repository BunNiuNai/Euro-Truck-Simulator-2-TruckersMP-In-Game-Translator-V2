// JSON 解析器的自测入口。
//
// 这是 ADR-005「单元要小而自证」的落地：本文件不依赖任何测试框架，
// 编译成独立 exe，跑完打印结果并以退出码表明成败。
//
//   translator_native_tests.exe        → 全部通过返回 0
#include "../src/json.hpp"

#include <cstdio>
#include <string>

namespace {

int g_failed = 0;
int g_passed = 0;

void check(bool cond, const std::string& what) {
    if (cond) {
        ++g_passed;
        return;
    }
    ++g_failed;
    std::printf("  FAIL: %s\n", what.c_str());
}

void checkEq(const std::string& got, const std::string& want, const std::string& what) {
    if (got == want) {
        ++g_passed;
        return;
    }
    ++g_failed;
    std::printf("  FAIL: %s\n        got  = %s\n        want = %s\n",
                what.c_str(), got.c_str(), want.c_str());
}

void checkEqInt(int got, int want, const std::string& what) {
    if (got == want) {
        ++g_passed;
        return;
    }
    ++g_failed;
    std::printf("  FAIL: %s  got=%d want=%d\n", what.c_str(), got, want);
}

// 解析成功并返回该值
bool parseOk(const std::string& text, tn::Json& out) {
    std::string err;
    if (!tn::Json::parse(text, out, err)) {
        std::printf("  FAIL: 解析 %s 失败: %s\n", text.c_str(), err.c_str());
        ++g_failed;
        return false;
    }
    ++g_passed;
    return true;
}

void testBasicObject() {
    tn::Json j;
    if (!parseOk(R"({"type":"native.hello","id":7,"payload":{"protocolVersion":1}})", j)) return;

    check(j.isObject(), "顶层应是对象");
    checkEq(j.find("type")->asString(), "native.hello", "type 字段");
    checkEqInt(j.find("id")->asInt(), 7, "id 字段");
    const tn::Json* payload = j.find("payload");
    check(payload != nullptr && payload->isObject(), "payload 应是对象");
    checkEqInt(payload->find("protocolVersion")->asInt(), 1, "protocolVersion");
    check(j.find("不存在") == nullptr, "缺失键应返回 nullptr");
}

void testFindOnNonObject() {
    tn::Json j;
    if (!parseOk("[1,2,3]", j)) return;
    check(j.find("x") == nullptr, "对数组 find 应返回 nullptr");
    checkEqInt(static_cast<int>(j.array.size()), 3, "数组长度");
}

void testStringEscapes() {
    tn::Json j;
    if (!parseOk(R"({"s":"a\"b\\c\/d\be\ff\ng\rh\ti"})", j)) return;
    checkEq(j.find("s")->asString(), "a\"b\\c/d\be\ff\ng\rh\ti", "转义序列");
}

void testUnicodeEscape() {
    tn::Json j;
    // \u4f60\u597d = 你好
    if (!parseOk(R"({"s":"\u4f60\u597d"})", j)) return;
    checkEq(j.find("s")->asString(), "你好", "\\u 基本平面");

    // \uD83D\uDE00 = 😀（代理对，4 字节 UTF-8）
    tn::Json k;
    if (!parseOk(R"({"s":"\uD83D\uDE00"})", k)) return;
    checkEq(k.find("s")->asString(), "😀", "\\u 代理对");
}

void testRawUtf8Passthrough() {
    tn::Json j;
    if (!parseOk(R"({"text":"你好 where are you"})", j)) return;
    checkEq(j.find("text")->asString(), "你好 where are you", "原样 UTF-8");
}

void testNumbers() {
    tn::Json j;
    if (!parseOk(R"({"a":0,"b":-12,"c":3.5,"d":1e3,"e":-2.5e-2})", j)) return;
    checkEqInt(j.find("a")->asInt(), 0, "整数 0");
    checkEqInt(j.find("b")->asInt(), -12, "负数");
    check(j.find("c")->asNumber() > 3.49 && j.find("c")->asNumber() < 3.51, "小数");
    check(j.find("d")->asNumber() > 999 && j.find("d")->asNumber() < 1001, "指数");
    check(j.find("e")->asNumber() < 0, "负指数");
}

void testLiterals() {
    tn::Json j;
    if (!parseOk(R"({"t":true,"f":false,"n":null})", j)) return;
    check(j.find("t")->asBool() == true, "true");
    check(!j.find("f")->asBool(), "false 应为 false");
    check(j.find("n")->isNull(), "null");
}

void testNested() {
    tn::Json j;
    if (!parseOk(R"({"list":[{"id":"a","combo":"shift+y"},{"id":"b","combo":"y"}]})", j)) return;
    const tn::Json* list = j.find("list");
    check(list != nullptr && list->isArray(), "list 是数组");
    checkEqInt(static_cast<int>(list->array.size()), 2, "数组元素数");
    checkEq(list->array[1].find("combo")->asString(), "y", "嵌套取值");
}

void testWhitespace() {
    tn::Json j;
    if (!parseOk("  {\n\t\"a\" :\r\n 1 ,\n \"b\" : [ 1 , 2 ]\n}  ", j)) return;
    checkEqInt(j.find("a")->asInt(), 1, "含空白解析");
    checkEqInt(static_cast<int>(j.find("b")->array.size()), 2, "数组含空白");
}

// ── 错误路径：必须返回 false 且不崩溃 ──

void expectFail(const std::string& text, const std::string& what) {
    tn::Json j;
    std::string err;
    if (tn::Json::parse(text, j, err)) {
        ++g_failed;
        std::printf("  FAIL: %s —— 本应解析失败，却成功了\n", what.c_str());
        return;
    }
    if (err.empty()) {
        ++g_failed;
        std::printf("  FAIL: %s —— 失败但没给出错误描述\n", what.c_str());
        return;
    }
    ++g_passed;
}

void testErrorCases() {
    expectFail("", "空文本");
    expectFail("{", "对象未闭合");
    expectFail("[1,2", "数组未闭合");
    expectFail("{\"a\":}", "缺少值");
    expectFail("{\"a\" 1}", "缺少冒号");
    expectFail("{\"a\":1,}", "对象尾随逗号");
    expectFail("[1,]", "数组尾随逗号");
    expectFail("\"未闭合", "字符串未闭合");
    expectFail("{\"a\":tru}", "非法字面量");
    expectFail("{\"a\":1}x", "结尾多余内容");
    expectFail("{\"a\":\"x\ty\"}", "字符串含未转义控制字符");
    expectFail("{\"a\":\"\\q\"}", "未知转义");
    expectFail("{\"a\":\"\\u12\"}", "\\u 位数不足");
    expectFail("{\"a\":01}", "数字前导零");
    expectFail("{\"a\":-}", "只有负号");
    expectFail("{\"a\":.5}", "缺少整数部分");
    expectFail("{\"a\":1.}", "小数点后无数字");
    expectFail("{\"a\":1e}", "指数后无数字");
    expectFail("{\"a\":1e+}", "指数符号后无数字");
}

// 合法数字边界（修正前导零后不能误伤这些）
void testNumberBoundaries() {
    tn::Json j;
    if (!parseOk(R"({"a":0,"b":-0,"c":0.5,"d":10,"e":1e10,"f":-1.25e+3})", j)) return;
    checkEqInt(j.find("a")->asInt(), 0, "0 合法");
    checkEqInt(j.find("d")->asInt(), 10, "10 合法（前导零规则不影响）");
    check(j.find("c")->asNumber() > 0.49, "0.5 合法");
    check(j.find("e")->asNumber() > 1e9, "1e10 合法");
    check(j.find("f")->asNumber() < 0, "-1.25e+3 合法");
}

void testDepthLimit() {
    // 构造 100 层嵌套，应因为深度限制而失败而不是栈溢出
    std::string deep;
    for (int i = 0; i < 100; ++i) deep += "[";
    for (int i = 0; i < 100; ++i) deep += "]";
    expectFail(deep, "超过深度限制");
}

// ── 序列化 ──

void testDumpRoundTrip() {
    tn::Json j;
    if (!parseOk(R"({"type":"event.hotkey","id":3,"payload":{"id":"send","text":"你好\n\"引号\""}})", j)) return;

    const std::string dumped = j.dump();
    tn::Json again;
    if (!parseOk(dumped, again)) return;

    checkEq(again.find("type")->asString(), "event.hotkey", "往返 type");
    checkEqInt(again.find("id")->asInt(), 3, "往返 id");
    const tn::Json* p = again.find("payload");
    checkEq(p->find("text")->asString(), "你好\n\"引号\"", "往返含换行与引号的文本");
}

void testDumpEscapes() {
    tn::Json obj = tn::Json::makeObject();
    obj.set("s", tn::Json("tab\there\nnewline"));
    const std::string dumped = obj.dump();
    check(dumped.find("\\t") != std::string::npos, "制表符应被转义");
    check(dumped.find("\\n") != std::string::npos, "换行应被转义");
    check(dumped.find('\n') == std::string::npos, "dump 结果不应含裸换行（否则会破坏 IPC 帧）");
}

void testBuildHelpers() {
    tn::Json root = tn::Json::makeObject();
    root.set("type", tn::Json("native.ready"));
    root.set("id", tn::Json(1.0));

    tn::Json caps = tn::Json::makeArray();
    caps.push(tn::Json("hotkey"));
    caps.push(tn::Json("tray"));
    root.set("capabilities", caps);

    const std::string dumped = root.dump();
    tn::Json again;
    if (!parseOk(dumped, again)) return;
    checkEq(again.find("type")->asString(), "native.ready", "构建的 type");
    checkEqInt(static_cast<int>(again.find("capabilities")->array.size()), 2, "构建的数组");
}

void testUtf8Encoding() {
    std::string out;
    tn::appendUtf8(out, 0x41);     // 'A'
    checkEq(out, "A", "ASCII 编码");

    out.clear();
    tn::appendUtf8(out, 0x4F60);   // 你
    checkEq(out, "你", "3 字节 UTF-8");

    out.clear();
    tn::appendUtf8(out, 0x1F600);  // 😀
    checkEq(out, "😀", "4 字节 UTF-8");
}

}  // namespace

int main() {
    std::printf("=== translator_native JSON 自测 ===\n");

    testBasicObject();
    testFindOnNonObject();
    testStringEscapes();
    testUnicodeEscape();
    testRawUtf8Passthrough();
    testNumbers();
    testLiterals();
    testNested();
    testWhitespace();
    testErrorCases();
    testNumberBoundaries();
    testDepthLimit();
    testDumpRoundTrip();
    testDumpEscapes();
    testBuildHelpers();
    testUtf8Encoding();

    std::printf("通过 %d 项，失败 %d 项\n", g_passed, g_failed);
    if (g_failed == 0) {
        std::printf("=== 全部通过 ===\n");
        return 0;
    }
    std::printf("=== 存在失败 ===\n");
    return 1;
}
