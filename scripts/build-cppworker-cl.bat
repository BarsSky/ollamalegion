@echo off
setlocal
call "C:\Program Files\Microsoft Visual Studio\18\Community\VC\Auxiliary\Build\vcvarsall.bat" x64 >nul 2>&1
if errorlevel 1 (
  echo [ERROR] vcvarsall.bat failed
  exit /b 1
)

set CC=cl.exe
set CXX=cl.exe
set CGO_ENABLED=1
set CGO_CFLAGS=-IC:\Ollama\ollamalegion\c\llama.cpp\include -IC:\Ollama\ollamalegion\c\llama.cpp\ggml\include
set CGO_CXXFLAGS=-std=c++17
set CGO_LDFLAGS=

cd /d "C:\Ollama\ollamalegion"
echo [BUILD] cppworker with cl.exe as CC/CXX
go build -ldflags "-s -w" -o bin\cppworker.exe .\cmd\cppworker 2>&1
exit /b %errorlevel%
