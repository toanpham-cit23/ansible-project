FROM python:3.12-alpine

RUN apk add --no-cache sqlite-libs tini \
 && pip install --no-cache-dir fastapi==0.115.* uvicorn==0.32.* cryptography==43.* paramiko==3.5.*

ENV PYTHONOPTIMIZE=2 \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PYTHONMALLOC=malloc

WORKDIR /app
COPY app.py .
COPY static ./static

RUN adduser -D -u 10001 app && chown -R app:app /app
USER app
EXPOSE 8000

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8000", \
     "--workers", "1", "--no-access-log", "--no-server-header", \
     "--timeout-keep-alive", "5", "--limit-concurrency", "64"]
