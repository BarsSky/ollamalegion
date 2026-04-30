FROM python:3.11-slim
RUN apt-get update && apt-get install -y curl && rm -rf /var/lib/apt/lists/*
COPY mock_extended.py /mock_extended.py
EXPOSE 11434
CMD ["python", "/mock_extended.py", "11434"]
