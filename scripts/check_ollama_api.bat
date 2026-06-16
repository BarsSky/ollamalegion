@echo off
setlocal enabledelayedexpansion
set BASE=http://127.0.0.1:18080
set MODEL=gemma-4-E4B-it-Q4_K_M.gguf

echo === Ollama API endpoint check via balancer (%BASE%) ===
echo.

call :get "/api/version"
call :get "/api/tags"
call :get "/api/ps"

call :post "/api/show" "{\"model\":\"%MODEL%\"}"
call :post "/api/generate" "{\"model\":\"%MODEL%\",\"prompt\":\"Say yes if you work.\",\"stream\":false,\"options\":{\"num_predict\":5,\"temperature\":0.1}}" 60
call :post "/api/chat" "{\"model\":\"%MODEL%\",\"messages\":[{\"role\":\"user\",\"content\":\"Say yes if you work.\"}],\"stream\":false,\"options\":{\"num_predict\":5,\"temperature\":0.1}}" 60
call :post "/api/embeddings" "{\"model\":\"%MODEL%\",\"prompt\":\"hello world\"}" 60
call :post "/api/pull" "{\"name\":\"hf:dummy/dummy.gguf\"}" 10
call :post "/api/delete" "{\"name\":\"%MODEL%\"}" 10
call :post "/api/copy" "{\"source\":\"%MODEL%\",\"destination\":\"copy-test.gguf\"}" 10
call :post "/api/create" "{\"name\":\"test-model\",\"modelfile\":\"FROM %MODEL%\"}" 10

echo.
echo === Done ===
exit /b

:get
echo [GET] %BASE%%~1
curl -s -w " HTTP_STATUS:%%{http_code}" --max-time 30 %BASE%%~1
echo.
echo.
exit /b

:post
echo [POST] %BASE%%~1
if "%~3"=="" (set TMO=30) else (set TMO=%~3)
curl -s -w " HTTP_STATUS:%%{http_code}" --max-time %TMO% -X POST -H "Content-Type: application/json" -d "%~2" %BASE%%~1
echo.
echo.
exit /b
