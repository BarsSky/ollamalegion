# tests/CMakeLists.txt — regression tests для c/bridge.
#
# Эти тесты компилируются в c/llama.cpp build (parent CMakeLists) через
# enable_testing() и add_subdirectory(common/tests) — но c/bridge — отдельный
# subproject со своим CMakeLists.txt.
#
# R66d (2026-09-22): теперь все тесты зарегистрированы через add_test(), и CI
# (job test-c-bridge) запускает их ОДНОЙ командой `ctest --output-on-failure`.
# Раньше job собирал и запускал только test_batch_n_tokens, а test_batched_batch,
# test_abort_api, test_embeddings_mode и abort_syntax_test не запускались вообще —
# регрессии в abort API и в batched-батче проходили мимо CI.
#
# Локальный запуск:
#   cmake -DCOMMON_LIB=... -DLLAMA_LIB=... -DGGML_LIBS=... ..
#   make && ctest --output-on-failure

cmake_minimum_required(VERSION 3.14)
project(bridge_tests C)

enable_testing()

# R66c (2026-09-22): заголовки llama.cpp для таргетов, компилирующих ../bridge.c.
# bridge_internal.h делает `#include "llama.h"`, а конфигурация запускается из
# c/bridge/tests/build — относительный путь сам не разрешается, и сборка падала:
#   bridge_internal.h:13:10: fatal error: llama.h: No such file or directory
# (прогон 35738576072, job test-c-bridge: путь к libllama-common.a уже починен,
# после чего компиляция дошла до этой ошибки).
# От c/bridge/tests до c/llama.cpp — два уровня вверх.
set(LLAMA_CPP_INCLUDE_DIRS
    ${CMAKE_CURRENT_SOURCE_DIR}/../../llama.cpp/include
    ${CMAKE_CURRENT_SOURCE_DIR}/../../llama.cpp/ggml/include
)

# bridge_link_llama — линковка теста, который дергает llama.cpp.
#
# R66c (2026-09-22): libllama-common.a НЕ содержит ядра: llama_batch_init/
# llama_batch_free/llama_get_memory/llama_memory_clear живут в libllama.a
# (+ libggml*.a). Линкуем их одной группой — у статических библиотек llama.cpp
# циклические зависимости. stdc++ нужен, потому что llama.cpp написан на C++,
# а тест — на C; gomp — потому что ggml-cpu собран с OpenMP
# (иначе undefined reference на GOMP_barrier/GOMP_parallel/omp_get_*).
#
# R66d (2026-09-22): вынесено в функцию, потому что test_batched_batch линковался
# только с ${COMMON_LIB} — при первом же прогоне в CI (после включения всех
# тестов) это дало бы тот же класс ошибок, что раньше у test_batch_n_tokens:
# undefined reference to `llama_batch_free', `ggml_*', `GOMP_*'.
function(bridge_link_llama target)
    if(DEFINED COMMON_LIB)
        if(DEFINED LLAMA_LIB)
            target_link_libraries(${target} PRIVATE
                -Wl,--start-group ${COMMON_LIB} ${LLAMA_LIB} ${GGML_LIBS} -Wl,--end-group
                stdc++ m pthread gomp)
        else()
            target_link_libraries(${target} PRIVATE ${COMMON_LIB})
        endif()
        return()
    endif()

    # Fallback: попробуй auto-detect (для разработки вне Docker)
    find_library(COMMON_LIB_AUTO llama-common
        PATHS
        ${CMAKE_CURRENT_SOURCE_DIR}/../../llama.cpp/build/common
        ${CMAKE_CURRENT_SOURCE_DIR}/../../build/common
        NO_DEFAULT_PATH
    )
    if(COMMON_LIB_AUTO)
        target_link_libraries(${target} PRIVATE ${COMMON_LIB_AUTO})
        message(STATUS "${target}: auto-detected libllama-common.a at ${COMMON_LIB_AUTO}")
    else()
        message(WARNING "${target}: libllama-common.a not found, link may fail")
    endif()
endfunction()

# ============================================================
# test_batch_n_tokens — regression test для Round 13 build_batch_with_seq bug
# (batch.n_tokens не выставлен → llama_decode fails).
#
# Линкуется против libllama-common.a + libllama.a + ggml и против
# c/bridge/bridge.c напрямую (для доступа к build_batch_with_seq через
# bridge_internal.h).
# ============================================================
add_executable(test_batch_n_tokens
    test_batch_n_tokens.c
    ../bridge.c
)
target_include_directories(test_batch_n_tokens PRIVATE
    ${CMAKE_SOURCE_DIR}/..
    ${CMAKE_CURRENT_SOURCE_DIR}/../..
    ${LLAMA_CPP_INCLUDE_DIRS}
)
bridge_link_llama(test_batch_n_tokens)
target_compile_features(test_batch_n_tokens PRIVATE c_std_99)

# ============================================================
# test_batched_batch — Round 15.1 regression test для build_batched_batch
# (true batched parallel inference — N sequences в ОДНОМ llama_batch).
# ============================================================
add_executable(test_batched_batch
    test_batched_batch.c
    ../bridge.c
)
target_include_directories(test_batched_batch PRIVATE
    ${CMAKE_SOURCE_DIR}/..
    ${CMAKE_CURRENT_SOURCE_DIR}/../..
    ${LLAMA_CPP_INCLUDE_DIRS}
)
bridge_link_llama(test_batched_batch)
target_compile_features(test_batched_batch PRIVATE c_std_99)

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
target_compile_features(test_abort_api PRIVATE c_std_99)

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
target_compile_features(test_embeddings_mode PRIVATE c_std_99)

# ============================================================
# abort_syntax_test — Round 5c: standalone-проверка, что abort API
# (bridge_request_abort / bridge_is_aborted / BRIDGE_ERR_ABORTED) объявлен
# ровно так, как ожидает Go-сторона (abort_syntax_check.h — шим-объявление).
#
# R66d (2026-09-22): раньше файл лежал в tests/ БЕЗ цели в CMake, то есть не
# собирался и не запускался ни разу (ни локально, ни в CI).
# ============================================================
add_executable(abort_syntax_test
    abort_syntax_test.c
)
target_include_directories(abort_syntax_test PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}
    ${CMAKE_SOURCE_DIR}/..
)
target_compile_features(abort_syntax_test PRIVATE c_std_99)

# ============================================================
# Регистрация в ctest — единая точка входа для CI.
# ============================================================
add_test(NAME test_batch_n_tokens COMMAND test_batch_n_tokens)
add_test(NAME test_batched_batch COMMAND test_batched_batch)
add_test(NAME test_abort_api COMMAND test_abort_api)
add_test(NAME test_embeddings_mode COMMAND test_embeddings_mode)
add_test(NAME abort_syntax_test COMMAND abort_syntax_test)

# Smoke target: собрать и прогнать всё одной командой `make bridge-tests`.
# R66d: в DEPENDS раньше не было test_abort_api — при параллельной сборке
# (`make -j`) бинарь мог не успеть собраться до запуска.
add_custom_target(bridge-tests
    COMMAND ${CMAKE_CTEST_COMMAND} --output-on-failure
    DEPENDS test_batch_n_tokens test_batched_batch test_abort_api
            test_embeddings_mode abort_syntax_test
    WORKING_DIRECTORY ${CMAKE_BINARY_DIR}
    COMMENT "Running bridge regression tests (ctest)"
)
