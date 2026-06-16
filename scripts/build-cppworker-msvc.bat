@echo off
setlocal
call "C:\Program Files\Microsoft Visual Studio\18\Community\VC\Auxiliary\Build\vcvarsall.bat" x64 >nul 2>&1
if errorlevel 1 (
  echo [ERROR] vcvarsall.bat failed
  exit /b 1
)

for /f "delims=" %%v in ('dir /b "C:\Program Files\NVIDIA GPU Computing Toolkit\CUDA" /ad') do set CUDA_LATEST=%%v
set CUDA_PATH=C:\Program Files\NVIDIA GPU Computing Toolkit\CUDA\%CUDA_LATEST%
set CGO_CFLAGS=-I"C:\Ollama\ollamalegion\c\llama.cpp\include" -I"C:\Ollama\ollamalegion\c\llama.cpp\ggml\include" -I"%CUDA_PATH%\include"
set CGO_CXXFLAGS=-std=c++17
set CGO_LDFLAGS=-L"C:\Ollama\ollamalegion\c\llama.cpp" -lllama -lggml-cpu -lggml -lggml-base -lllama-common -lllama-common-base -lcpp-httplib -L"%CUDA_PATH%\lib\x64" -lcudart -lcublas -lcublasLt -lws2_32 -lbcrypt
echo [BUILD] cppworker via CGo + MSVC
cd /d "C:\Ollama\ollamalegion"
go build -tags "" -ldflags "-s -w -extldflags=-static" -o bin\cppworker.exe -x .\cmd\cppworker
exit /b %errorlevel%
