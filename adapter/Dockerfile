FROM python:3.11-slim

WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY adapter.py .

EXPOSE 9100
CMD ["uvicorn", "adapter:app", "--host", "0.0.0.0", "--port", "9100"]
