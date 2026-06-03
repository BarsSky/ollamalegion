# CMakeLists.txt — сборка bridge (C-обёртка над llama.cpp)
# Линкуется с libllama.a + libggml.a из c/llama.cpp/build

cmake_minimum_required(VERSION 3.14)
project(ollamalegion_bridge C)

set(CMAKE_C_STANDARD 11)
set(CMAKE_C_STANDARD_REQUIRED ON)
set(CMAKE_POSITION_INDEPENDENT_CODE ON)

# Пути к llama.cpp
set(LLAMA_CPP_ROOT "${CMAKE_CURRENT_SOURCE_DIR}/../llama.cpp")
set(LLAMA_BUILD_DIR "${LLAMA_CPP_ROOT}/build")

# Ищем llama в сборочной директории
find_library(LLAMA_LIB llama PATHS "${LLAMA_BUILD_DIR}" "${LLAMA_BUILD_DIR}/src" NO_DEFAULT_PATH)
find_library(GGML_LIB ggml PATHS "${LLAMA_BUILD_DIR}" "${LLAMA_BUILD_DIR}/ggml/src" NO_DEFAULT_PATH)
find_library(GGML_CPU_LIB ggml-cpu PATHS "${LLAMA_BUILD_DIR}" "${LLAMA_BUILD_DIR}/ggml/src" NO_DEFAULT_PATH)
find_library(GGML_BASE_LIB ggml-base PATHS "${LLAMA_BUILD_DIR}" "${LLAMA_BUILD_DIR}/ggml/src" NO_DEFAULT_PATH)

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

message(STATUS "LLAMA_LIB: ${LLAMA_LIB}")
message(STATUS "GGML_LIB: ${GGML_LIB}")
message(STATUS "GGML_CPU_LIB: ${GGML_CPU_LIB}")
message(STATUS "GGML_BASE_LIB: ${GGML_BASE_LIB}")

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
set(BRIDGE_SOURCES
    bridge.c
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