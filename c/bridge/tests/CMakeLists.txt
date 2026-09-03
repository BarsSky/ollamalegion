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

# ============================================================
# test_batched_batch — Round 15.1 regression test для build_batched_batch
# (true batched parallel inference — N sequences в ОДНОМ llama_batch).
# ============================================================
# Аналогично test_batch_n_tokens, линкуется против bridge.c напрямую
# (для доступа к internal build_batched_batch через bridge_internal.h)
# и против libllama-common.a (для llama_batch_init, llama_batch_free).
add_executable(test_batched_batch
    test_batched_batch.c
    ../bridge.c
)
target_include_directories(test_batched_batch PRIVATE
    ${CMAKE_SOURCE_DIR}/..
    ${CMAKE_CURRENT_SOURCE_DIR}/../..
)
target_link_libraries(test_batched_batch PRIVATE ${COMMON_LIB})
target_compile_features(test_batched_batch PRIVATE C99)

# ============================================================
# test_abort_api — Round 31 #6: standalone unit tests для abort API.
#
# Standalone (без libllama): gcc -DTEST_ABORT_STANDALONE -o test_abort_api test_abort_api.c -lpthread
# Покрывает T1, T2, T7, T8, T9, T10, T11 (не требует libllama).
#
# С libllama (для T3-T6 — abort during long gen, abort+retry, 10 concurrent,
# 100 goroutines race): линковать с bridge.c + libllama-common.a.
# В standalone-режиме T3-T6 помечены как #if 0.
# ============================================================
add_executable(test_abort_api
    test_abort_api.c
)
target_include_directories(test_abort_api PRIVATE
    ${CMAKE_SOURCE_DIR}/..
    ${CMAKE_CURRENT_SOURCE_DIR}/../..
)
# Standalone build (по умолчанию). T3-T6 требуют libllama.
target_compile_definitions(test_abort_api PRIVATE TEST_ABORT_STANDALONE)
target_link_libraries(test_abort_api PRIVATE pthread)
target_compile_features(test_abort_api PRIVATE C99)

# Smoke test target (alias для удобства) — обновлён для запуска ВСЕХ тестов
add_custom_target(bridge-tests
    COMMAND test_batch_n_tokens && test_batched_batch && test_abort_api && test_embeddings_mode
    DEPENDS test_batch_n_tokens test_batched_batch test_embeddings_mode
    WORKING_DIRECTORY ${CMAKE_BINARY_DIR}
    COMMENT "Running bridge regression tests"
)

# ============================================================
# test_embeddings_mode — R60/Round 39: standalone unit tests для
# bridge_set_embeddings_mode API.
#
# Standalone (без libllama): gcc -o test_embeddings_mode test_embeddings_mode.c
# Покрывает T1 (NULL handle), T2 (defensive: NULL+valid mode). Реальный
# toggle chat↔embedding покрыт e2e test tests/cppworker_embeddings_mode_e2e.py
# (требует running cppworker + log scraping).
# ============================================================
add_executable(test_embeddings_mode
    test_embeddings_mode.c
)
target_compile_features(test_embeddings_mode PRIVATE C99)
