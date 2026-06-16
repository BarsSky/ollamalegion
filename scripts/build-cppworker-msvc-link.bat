@echo off
setlocal
call "C:\Program Files\Microsoft Visual Studio\18\Community\VC\Auxiliary\Build\vcvarsall.bat" x64 >nul 2>&1
if errorlevel 1 (
  echo [ERROR] vcvarsall.bat failed
  exit /b 1
)

set CGO_CFLAGS=-IC:\Ollama\ollamalegion\c\llama.cpp\include -IC:\Ollama\ollamalegion\c\llama.cpp\ggml\include
set CGO_CXXFLAGS=-std=c++17
set CGO_LDFLAGS=-LC:\Ollama\ollamalegion\c\llama.cpp -Wl,--start-group -lllama -lggml-cpu -lggml -lggml-base -lllama-common -lllama-common-base -lcpp-httplib -LC:\CUDA13\lib\x64 -lcudart -lcublas -lcublasLt -lws2_32 -lbcrypt -Wl,--end-group

cd /d "C:\Ollama\ollamalegion"
echo [BUILD] cppworker via gcc CGo + MSVC link.exe
go build -ldflags "-s -w -extld=cl.exe -extldflags=/LIBPATH:C:\Ollama\ollamalegion\c\llama.cpp\build-msvc-cuda\src /LIBPATH:C:\Ollama\ollamalegion\c\llama.cpp\build-msvc-cuda\ggml\src" -o bin\cppworker.exe .\cmd\cppworker 2>&1
exit /b %errorlevel%
