# tests/CMakeLists.txt — regression tests для c/bridge.
#
# Эти тесты компилируются в c/llama.cpp build (parent CMakeLists) через
# enable_testing() и add_subdirectory(common/tests) — но c/bridge — отдельный
# subproject со своим CMakeLists.txt.
#
# Сейчас тесты нужно запускать вручную из c/bridge/build/:
#   cmake ../.. -DCOMMON_LIB=$(realpath ../../llama.cpp/build/common/libllama-common.a)
#   make test_batch_n_tokens
#   ./test_batch_n_tokens
#
# TODO (Round 5c): integrate в c/llama.cpp/tests/CMakeLists.txt или в CI workflow
# чтобы они запускались автоматически при каждом build.

cmake_minimum_required(VERSION 3.14)
project(bridge_tests C)

# test_batch_n_tokens — regression test для Round 13 build_batch_with_seq bug
# (batch.n_tokens не выставлен → llama_decode fails).
#
# Этот тест линкуется против libllama-common.a (которая содержит llama_batch_init,
# llama_batch_free, и т.д.) и против c/bridge/bridge.c напрямую (для доступа к
# build_batch_with_seq через bridge_internal.h).

add_executable(test_batch_n_tokens
    test_batch_n_tokens.c
    ../bridge.c
)

# Headers: bridge.h, bridge_internal.h, llama.h, и internal llama.cpp headers
target_include_directories(test_batch_n_tokens PRIVATE
    ${CMAKE_SOURCE_DIR}/..
    ${CMAKE_CURRENT_SOURCE_DIR}/../..
)

# Link libllama-common.a (для llama_batch_init, llama_batch_free)
# Передаётся через -DCOMMON_LIB=/path/to/libllama-common.a при cmake
if(DEFINED COMMON_LIB)
    target_link_libraries(test_batch_n_tokens PRIVATE ${COMMON_LIB})
else()
    # Fallback: попробуй auto-detect (для разработки вне Docker)
    find_library(COMMON_LIB_AUTO llama-common
        PATHS
        ${CMAKE_CURRENT_SOURCE_DIR}/../../llama.cpp/build/common
        ${CMAKE_CURRENT_SOURCE_DIR}/../../build/common
        NO_DEFAULT_PATH
    )
    if(COMMON_LIB_AUTO)
        target_link_libraries(test_batch_n_tokens PRIVATE ${COMMON_LIB_AUTO})
        message(STATUS "test_batch_n_tokens: auto-detected libllama-common.a at ${COMMON_LIB_AUTO}")
    else()
        message(WARNING "test_batch_n_tokens: libllama-common.a not found, link may fail")
    endif()
endif()

# C standard для bridge.c
target_compile_features(test_batch_n_tokens PRIVATE C99)

# Smoke test target (alias для удобства)
add_custom_target(bridge-tests
    COMMAND test_batch_n_tokens
    DEPENDS test_batch_n_tokens
    WORKING_DIRECTORY ${CMAKE_BINARY_DIR}
    COMMENT "Running bridge regression tests"
)
