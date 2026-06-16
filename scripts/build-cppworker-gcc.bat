@echo off
setlocal
set CUDA_PATH=C:\Program Files\NVIDIA GPU Computing Toolkit\CUDA\v13.3
set CGO_CFLAGS=-I"C:\Ollama\ollamalegion\c\llama.cpp\include" -I"C:\Ollama\ollamalegion\c\llama.cpp\ggml\include" -I"%CUDA_PATH%\include"
set CGO_CXXFLAGS=-std=c++17
set CGO_LDFLAGS=-L"C:\Ollama\ollamalegion\c\llama.cpp" -Wl,--start-group -lllama -lggml-cpu -lggml -lggml-base -lllama-common -lllama-common-base -lcpp-httplib -L"%CUDA_PATH%\lib\x64" -lcudart -lcublas -lcublasLt -lws2_32 -lbcrypt -Wl,--end-group
echo [BUILD] cppworker via gcc CGo + MSVC .lib
cd /d "C:\Ollama\ollamalegion"
go build -ldflags "-s -w" -o bin\cppworker.exe .\cmd\cppworker
exit /b %errorlevel%
