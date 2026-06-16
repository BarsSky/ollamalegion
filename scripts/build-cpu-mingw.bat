@echo off
setlocal enabledelayedexpansion
echo === Building llama.cpp CPU-only with MinGW + CMake ===
cd /d "C:\Ollama\ollamalegion\c\llama.cpp"

if not exist build-cpu (
  mkdir build-cpu
)

cmake -S . -B build-cpu -G "MinGW Makefiles" ^
  -DCMAKE_BUILD_TYPE=Release ^
  -DGGML_NATIVE=OFF ^
  -DGGML_OPENMP=ON ^
  -DGGML_BLAS=OFF ^
  -DGGML_CUDA=OFF ^
  -DGGML_VULKAN=OFF ^
  -DGGML_HIP=OFF ^
  -DGGML_METAL=OFF ^
  -DGGML_CCACHE=OFF ^
  -DBUILD_SHARED_LIBS=OFF ^
  -DLLAMA_BUILD_TESTS=OFF ^
  -DLLAMA_BUILD_EXAMPLES=OFF ^
  -DLLAMA_BUILD_SERVER=OFF ^
  -DLLAMA_CURL=OFF ^
  -DCMAKE_C_FLAGS="-DGGML_BUGFIXES" ^
  -Wno-dev

if errorlevel 1 (
  echo [ERROR] CMake configure failed
  exit /b 1
)

cmake --build build-cpu --config Release -j%NUMBER_OF_PROCESSORS%
exit /b %errorlevel%
