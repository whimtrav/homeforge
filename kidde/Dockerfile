FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY kidde_bridge.py .
CMD ["python","-u","kidde_bridge.py"]
