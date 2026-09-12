# Translator V2 — Native (C++) 构建
#
# 骨架阶段刻意极简：只用 C++ 标准库，不引入 WebView2 / DirectX 依赖，
# 保证「装了 MSVC 就能编译通过」（ADR-005 组织方式约束）。
#
# 构建：
#   cmake -S . -B build -G "Visual Studio 17 2022" -A x64
#   cmake --build build --config Release
#
# 产物：
#   dist/translator_native.exe        主程序（阶段 3 起承载 WebView2）
#   dist/translator_native_tests.exe  自测入口（ADR-005「小而自证」要求）
#
# 后续会加入：pipe/(Named Pipe 服务) hotkey/ input/ tray/ window/ webview/

cmake_minimum_required(VERSION 3.20)
project(translator_native LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

if(NOT WIN32)
    message(FATAL_ERROR "translator_native 仅支持 Windows（ADR-014 / 非目标 N4）")
endif()

# ── WebView2：可选模块，SDK 不是构建硬依赖 ──────────────────────
#
# ADR-013 的补充决定：SDK 不得成为硬依赖，否则「装了 MSVC 就能编译」就破了。
# 做法是**条件接入**：
#   · 找到 SDK → 加头文件目录与导入库，并定义 TN_HAS_WEBVIEW2=1
#   · 找不到   → 只少了这些，webview.cpp 走它的降级分支，native 其余能力照常
# 用 tools/fetch-webview2-sdk.ps1 可以取回 SDK（需要网络）。
#
# 优先级用 Static.lib：静态链接就不必在运行时摆一个 WebView2Loader.dll 在旁边。
#
# 路径查找顺序（先显式后约定）：
#   1. -DTN_WEBVIEW2_SDK=<路径>
#   2. 环境变量 ETS2_WEBVIEW2_SDK
#   3. 工作区根/Translator-V2 下的 .sdk/webview2 与 third_party/webview2
set(TN_WEBVIEW2_SDK "" CACHE PATH "WebView2 SDK 根目录（其下有 build/native）")

if(NOT TN_WEBVIEW2_SDK AND DEFINED ENV{ETS2_WEBVIEW2_SDK})
    set(TN_WEBVIEW2_SDK "$ENV{ETS2_WEBVIEW2_SDK}")
endif()

if(NOT TN_WEBVIEW2_SDK)
    # 注意 ../.. ：SDK 默认放在**工作区根**（与 Translator-V2 同级），
    # 而不是 Translator-V2 内部——少写一级就会「明明装了却找不到」。
    foreach(candidate
            "${CMAKE_CURRENT_SOURCE_DIR}/../../.sdk/webview2"
            "${CMAKE_CURRENT_SOURCE_DIR}/../.sdk/webview2"
            "${CMAKE_CURRENT_SOURCE_DIR}/../../third_party/webview2"
            "${CMAKE_CURRENT_SOURCE_DIR}/../third_party/webview2")
        if(EXISTS "${candidate}/build/native/include/WebView2.h")
            set(TN_WEBVIEW2_SDK "${candidate}")
            break()
        endif()
    endforeach()
endif()

set(TN_WEBVIEW_INCLUDE "")
set(TN_WEBVIEW_LIB "")
if(TN_WEBVIEW2_SDK AND EXISTS "${TN_WEBVIEW2_SDK}/build/native/include/WebView2.h")
    set(TN_WEBVIEW_INCLUDE "${TN_WEBVIEW2_SDK}/build/native/include")
    set(TN_WEBVIEW_LIB "${TN_WEBVIEW2_SDK}/build/native/x64/WebView2LoaderStatic.lib")
    if(NOT EXISTS "${TN_WEBVIEW_LIB}")
        # 退而求其次：动态 loader 的导入库（运行时需要 WebView2Loader.dll 在旁边）
        set(TN_WEBVIEW_LIB "${TN_WEBVIEW2_SDK}/build/native/x64/WebView2Loader.dll.lib")
    endif()
    if(NOT EXISTS "${TN_WEBVIEW_LIB}")
        set(TN_WEBVIEW_INCLUDE "")
        message(WARNING "找到 WebView2 头文件但没有 x64 导入库，显示层不接进来")
    endif()
endif()

if(TN_WEBVIEW_INCLUDE)
    message(STATUS "WebView2 SDK: ${TN_WEBVIEW2_SDK}（显示层已接入）")
else()
    message(STATUS "未找到 WebView2 SDK —— 显示层降级（native 其余能力照常）")
endif()

# 共享源码：各单元保持独立可编译
#
# ⚠️ webview.cpp **始终**参与编译：它内部用 TN_HAS_WEBVIEW2 分成两半，
#    没有 SDK 时提供一套「未编译进显示层」的空实现。把它条件排除掉的话，
#    dispatch/main 里对 WebViewHost 的引用就会变成链接错误。
set(TN_CORE_SOURCES
    src/json.cpp
    src/pipe.cpp
    src/window.cpp
    src/input.cpp
    src/hotkey.cpp
    src/webview.cpp
    src/tray.cpp
    src/dispatch.cpp
)

add_executable(translator_native
    src/main.cpp
    ${TN_CORE_SOURCES}
)
target_include_directories(translator_native PRIVATE include src)

# 自测入口：不依赖任何测试框架，跑完以退出码表明成败
add_executable(translator_native_tests
    tests/test_json.cpp
    ${TN_CORE_SOURCES}
)
target_include_directories(translator_native_tests PRIVATE include src)

add_executable(translator_native_window_tests
    tests/test_window.cpp
    ${TN_CORE_SOURCES}
)
target_include_directories(translator_native_window_tests PRIVATE include src)

# 分发自测：直接喂 JSON 帧给 handleMessage，不依赖管道客户端
add_executable(translator_native_dispatch_tests
    tests/test_dispatch.cpp
    ${TN_CORE_SOURCES}
)
target_include_directories(translator_native_dispatch_tests PRIVATE include src)

# 输入自测：修饰键展开（纯函数）+ 真实剪贴板往返
add_executable(translator_native_input_tests
    tests/test_input.cpp
    ${TN_CORE_SOURCES}
)
target_include_directories(translator_native_input_tests PRIVATE include src)

# 热键自测：组合键匹配（纯函数）+ 注册/降级不变量
add_executable(translator_native_hotkey_tests
    tests/test_hotkey.cpp
    ${TN_CORE_SOURCES}
)
target_include_directories(translator_native_hotkey_tests PRIVATE include src)

# 托盘自测：线程生命周期（反复 create/destroy、窗口未发布就销毁、建窗失败后析构）
# 这一套必须单独成 exe：它会把托盘窗口类名临时注册到别的模块名下（人为制造
# 建窗失败），虽然用完立刻注销，但放进别的套件会污染同进程里的窗口测试。
add_executable(translator_native_tray_tests
    tests/test_tray.cpp
    ${TN_CORE_SOURCES}
)
target_include_directories(translator_native_tray_tests PRIVATE include src)

foreach(tgt translator_native translator_native_tests translator_native_window_tests
            translator_native_dispatch_tests translator_native_input_tests
            translator_native_hotkey_tests translator_native_tray_tests)
    # Win32 宽字符程序的标准做法：让 TCHAR 系列与 IDC_*/IDI_* 宏走宽字符版本
    target_compile_definitions(${tgt} PRIVATE UNICODE _UNICODE)
    if(MSVC)
        target_compile_options(${tgt} PRIVATE /W4 /utf-8)
    else()
        target_compile_options(${tgt} PRIVATE -Wall -Wextra)
    endif()

    # WebView2 只在找到 SDK 时接入；同时用宏告诉代码「这次构建有没有显示层」，
    # 好让 capabilities() 如实上报，而不是编译进一个残废的 webview 模块。
    if(TN_WEBVIEW_INCLUDE)
        target_include_directories(${tgt} PRIVATE "${TN_WEBVIEW_INCLUDE}")
        target_link_libraries(${tgt} PRIVATE "${TN_WEBVIEW_LIB}")
        target_compile_definitions(${tgt} PRIVATE TN_HAS_WEBVIEW2=1)
    endif()

    # 输出到 dist/，与 Go 侧打包脚本约定一致
    set_target_properties(${tgt} PROPERTIES
        RUNTIME_OUTPUT_DIRECTORY "${CMAKE_SOURCE_DIR}/../dist"
        RUNTIME_OUTPUT_DIRECTORY_DEBUG "${CMAKE_SOURCE_DIR}/../dist"
        RUNTIME_OUTPUT_DIRECTORY_RELEASE "${CMAKE_SOURCE_DIR}/../dist"
    )
endforeach()

# 让 `cmake --build build --config Release --target run-tests` 可直接跑全部自测
add_custom_target(run-tests
    COMMAND $<TARGET_FILE:translator_native_tests>
    COMMAND $<TARGET_FILE:translator_native_window_tests>
    COMMAND $<TARGET_FILE:translator_native_dispatch_tests>
    COMMAND $<TARGET_FILE:translator_native_input_tests>
    COMMAND $<TARGET_FILE:translator_native_hotkey_tests>
    COMMAND $<TARGET_FILE:translator_native_tray_tests>
    DEPENDS translator_native_tests translator_native_window_tests
            translator_native_dispatch_tests translator_native_input_tests
            translator_native_hotkey_tests translator_native_tray_tests
    COMMENT "运行 native 自测（json + window + dispatch + input + hotkey + tray）"
)

# ctest 接入。
#
# 各套件是**自建断言**的可执行文件，退出码即成败（0 通过），所以这里只需要
# 把它们注册进去。README 里给的验证命令一直是 `ctest --test-dir <build> -C Release`，
# 而 CMakeLists 里此前既没有 enable_testing 也没有 add_test，那条命令实际上
# 永远报「No tests were found」——命令与实现对不上，排障时很浪费时间。
enable_testing()

set(TN_TEST_TIMEOUT 300)

foreach(suite translator_native_tests translator_native_window_tests
              translator_native_dispatch_tests translator_native_input_tests
              translator_native_hotkey_tests translator_native_tray_tests)
    add_test(NAME ${suite} COMMAND ${suite})
    # 超时兜底：window 套件要拉起真实进程、tray 套件要等托盘线程，
    # 万一哪条路径卡住（比如 destroy 挂死），ctest 必须能判定失败而不是一直挂着。
    set_tests_properties(${suite} PROPERTIES TIMEOUT ${TN_TEST_TIMEOUT})
endforeach()
