# CMakeLists.txt — сборка bridge (C-обёртка над llama.cpp)
# Линкуется с libllama.a + libggml.a + libcommon.a из c/llama.cpp/build
#
# Round 14a (2026-07-28): добавлен C++ wrapper chat_thinking.cpp для
# native enable_thinking через common::chat. Project type изменён
# с C на CXX (mixed C/C++ source). CXX standard 17 требуется для
# std::string_view, std::variant, и других возможностей используемых
# в c/llama.cpp/common/.

cmake_minimum_required(VERSION 3.14)
project(ollamalegion_bridge CXX)

# Round 14a: enable both C and C++ standards.
# C source (bridge.c) использует C11 — CMAKE_C_STANDARD.
# C++ source (chat_thinking.cpp) использует C++17 (std::string, std::vector).
set(CMAKE_C_STANDARD 11)
set(CMAKE_C_STANDARD_REQUIRED ON)
set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_POSITION_INDEPENDENT_CODE ON)

# Пути к llama.cpp
set(LLAMA_CPP_ROOT "${CMAKE_CURRENT_SOURCE_DIR}/../llama.cpp")
set(LLAMA_BUILD_DIR "${LLAMA_CPP_ROOT}/build")

# Ищем llama в сборочной директории
find_library(LLAMA_LIB llama PATHS "${LLAMA_BUILD_DIR}" "${LLAMA_BUILD_DIR}/src" NO_DEFAULT_PATH)
find_library(GGML_LIB ggml PATHS "${LLAMA_BUILD_DIR}" "${LLAMA_BUILD_DIR}/ggml/src" NO_DEFAULT_PATH)
find_library(GGML_CPU_LIB ggml-cpu PATHS "${LLAMA_BUILD_DIR}" "${LLAMA_BUILD_DIR}/ggml/src" NO_DEFAULT_PATH)
find_library(GGML_BASE_LIB ggml-base PATHS "${LLAMA_BUILD_DIR}" "${LLAMA_BUILD_DIR}/ggml/src" NO_DEFAULT_PATH)
# Round 14a: libcommon.a — для C++ API common_chat_templates_apply и др.
find_library(COMMON_LIB common PATHS "${LLAMA_BUILD_DIR}/common" "${LLAMA_BUILD_DIR}" NO_DEFAULT_PATH)

# Если библиотеки не найдены, пробуем без NO_DEFAULT_PATH
if(NOT LLAMA_LIB)
    find_library(LLAMA_LIB llama PATHS "${LLAMA_BUILD_DIR}" "${LLAMA_BUILD_DIR}/src")
endif()
if(NOT GGML_LIB)
    find_library(GGML_LIB ggml PATHS "${LLAMA_BUILD_DIR}" "${LLAMA_BUILD_DIR}/ggml/src")
endif()
if(NOT GGML_CPU_LIB)
    find_library(GGML_CPU_LIB ggml-cpu PATHS "${LLAMA_BUILD_DIR}" "${LLAMA_BUILD_DIR}/ggml/src")
endif()
if(NOT GGML_BASE_LIB)
    find_library(GGML_BASE_LIB ggml-base PATHS "${LLAMA_BUILD_DIR}" "${LLAMA_BUILD_DIR}/ggml/src")
endif()
if(NOT COMMON_LIB)
    find_library(COMMON_LIB common PATHS "${LLAMA_BUILD_DIR}/common" "${LLAMA_BUILD_DIR}")
endif()

message(STATUS "LLAMA_LIB: ${LLAMA_LIB}")
message(STATUS "GGML_LIB: ${GGML_LIB}")
message(STATUS "GGML_CPU_LIB: ${GGML_CPU_LIB}")
message(STATUS "GGML_BASE_LIB: ${GGML_BASE_LIB}")
message(STATUS "COMMON_LIB (Round 14a): ${COMMON_LIB}")

# Проверка наличия CUDA
find_package(CUDAToolkit QUIET)
if(CUDAToolkit_FOUND)
    message(STATUS "CUDA found: ${CUDAToolkit_VERSION}")
    add_compile_definitions(GGML_USE_CUDA)
    set(CUDA_LIBS CUDA::cudart CUDA::cuda_driver)
else()
    message(STATUS "CUDA not found — building CPU-only bridge")
    set(CUDA_LIBS "")
endif()

# Исходники bridge
# Round 14a: добавлен csrc/chat_thinking.cpp (C++ wrapper для native enable_thinking).
# bridge.c остаётся C, csrc/chat_thinking.cpp — C++. Project type CXX (см. выше).
#
# csrc/ подпапка нужна чтобы Go build не пытался скомпилировать .cpp как
# Go source (Go сканирует все .go/.c/.cpp в package directory).
set(BRIDGE_SOURCES
    bridge.c
    csrc/chat_thinking.cpp
)

# Заголовки
set(BRIDGE_HEADERS
    bridge.h
)

# Собираем статическую библиотеку bridge
add_library(ollamalegion_bridge STATIC ${BRIDGE_SOURCES} ${BRIDGE_HEADERS})

# Include пути
target_include_directories(ollamalegion_bridge PUBLIC
    "${CMAKE_CURRENT_SOURCE_DIR}"
    "${CMAKE_CURRENT_SOURCE_DIR}/csrc"
    "${LLAMA_CPP_ROOT}/include"
    "${LLAMA_CPP_ROOT}/ggml/include"
    "${LLAMA_CPP_ROOT}/common"
)

# Линковка
set(LINK_LIBS "")
if(LLAMA_LIB)
    list(APPEND LINK_LIBS ${LLAMA_LIB})
endif()
if(GGML_LIB)
    list(APPEND LINK_LIBS ${GGML_LIB})
endif()
if(GGML_CPU_LIB)
    list(APPEND LINK_LIBS ${GGML_CPU_LIB})
endif()
if(GGML_BASE_LIB)
    list(APPEND LINK_LIBS ${GGML_BASE_LIB})
endif()
# Round 14a: libcommon.a содержит common_chat_templates_apply и другие
# C++ API для native enable_thinking support. Если common library не
# найдена (например, llama.cpp собран без неё), wrapper не компилируется
# — guard в самом начале CMakeLists (если нужен) мог бы skip target,
# но сейчас мы требуем наличие common для Round 14a.
if(COMMON_LIB)
    list(APPEND LINK_LIBS ${COMMON_LIB})
endif()
if(CUDA_LIBS)
    list(APPEND LINK_LIBS ${CUDA_LIBS})
endif()

# Стандартные библиотеки
if(UNIX)
    list(APPEND LINK_LIBS m pthread dl)
elseif(WIN32)
    list(APPEND LINK_LIBS ws2_32)
endif()

target_link_libraries(ollamalegion_bridge PUBLIC ${LINK_LIBS})

# Установка
install(TARGETS ollamalegion_bridge ARCHIVE DESTINATION lib)
install(FILES ${BRIDGE_HEADERS} DESTINATION include/ollamalegion)