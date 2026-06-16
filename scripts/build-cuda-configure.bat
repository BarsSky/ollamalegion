@echo off
setlocal EnableExtensions
call "C:\Program Files\Microsoft Visual Studio\18\Community\VC\Auxiliary\Build\vcvarsall.bat" x64
if errorlevel 1 (
    echo FAILED to activate vcvarsall
    exit /b 1
)

set "PATH=C:\Users\knaga\AppData\Local\Microsoft\WinGet\Packages\Ninja-build.Ninja_Microsoft.Winget.Source_8wekyb3d8bbwe;%PATH%"

if not exist "C:\Ollama\ollamalegion\c\llama.cpp\build-msvc-cuda" mkdir "C:\Ollama\ollamalegion\c\llama.cpp\build-msvc-cuda"
cd /d "C:\Ollama\ollamalegion\c\llama.cpp\build-msvc-cuda"

REM === Авто-детект последней установленной версии CUDA ===
set "CUDA_ROOT="
for /f "delims=" %%v in ('dir /b /ad "C:\Program Files\NVIDIA GPU Computing Toolkit\CUDA" 2^>nul ^| sort /r') do (
    if exist "C:\Program Files\NVIDIA GPU Computing Toolkit\CUDA\%%v\bin\nvcc.exe" (
        if not defined CUDA_ROOT set "CUDA_ROOT=C:\Program Files\NVIDIA GPU Computing Toolkit\CUDA\%%v"
    )
)
if defined CUDA_ROOT (
    set "PATH=%CUDA_ROOT%\bin;%PATH%"
    echo Using CUDA: %CUDA_ROOT%
)

echo === CMake configure ===
cmake -G Ninja ^
    -DCMAKE_BUILD_TYPE=Release ^
    -DGGML_CUDA=ON ^
    -DCMAKE_CUDA_ARCHITECTURES=86 ^
    -DGGML_OPENMP=OFF ^
    -DGGML_BLAS=OFF ^
    -DBUILD_SHARED_LIBS=OFF ^
    -DLLAMA_BUILD_TESTS=OFF ^
    -DLLAMA_BUILD_EXAMPLES=OFF ^
    -DLLAMA_BUILD_SERVER=OFF ^
    -DLLAMA_BUILD_TOOLS=OFF ^
    -DLLAMA_CURL=OFF ^
    "C:\Ollama\ollamalegion\c\llama.cpp"
if errorlevel 1 (
    echo FAILED cmake configure
    exit /b 1
)
echo === CMake configure SUCCESS ===
exit /b 0
