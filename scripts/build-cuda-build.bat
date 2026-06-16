@echo off
setlocal EnableExtensions

REM === Activate MSVC environment ===
call "C:\Program Files\Microsoft Visual Studio\18\Community\VC\Auxiliary\Build\vcvarsall.bat" x64
if errorlevel 1 (
    echo FAILED to activate vcvarsall
    exit /b 1
)

REM === Add Ninja to PATH ===
set "PATH=C:\Users\knaga\AppData\Local\Microsoft\WinGet\Packages\Ninja-build.Ninja_Microsoft.Winget.Source_8wekyb3d8bbwe;%PATH%"

REM === Build llama.cpp ===
cd /d "C:\Ollama\ollamalegion\c\llama.cpp\build-msvc-cuda"

echo === Starting build: llama.cpp + ggml + ggml-cuda (10-30 min) ===
cmake --build . --config Release --parallel 8
if errorlevel 1 (
    echo FAILED build
    exit /b 1
)
echo === Build SUCCESS ===
exit /b 0
