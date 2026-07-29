FROM python:3.11-slim
# libgomp1 is needed by onnxruntime (Piper's inference backend)
RUN apt-get update && apt-get install -y --no-install-recommends libgomp1 \
    && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir piper-tts flask
WORKDIR /app
COPY app.py /app/app.py
EXPOSE 5050
CMD ["python", "app.py"]
