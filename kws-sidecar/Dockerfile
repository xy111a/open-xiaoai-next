# KWS 边车：sherpa-onnx WenetSpeech 唤醒检测（服务端，NAS x86_64）
# 模型不 bake 进镜像：entrypoint 在模型缺失时下载（见 scripts/download-model.sh），
# 下载失败容器明确报错退出；模型持久化在 kws-data 卷。
FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    KWS_HOST=0.0.0.0 \
    KWS_PORT=10400 \
    KWS_MODEL_DIR=/data/kws/models

RUN apt-get update \
    && apt-get install -y --no-install-recommends curl bzip2 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY server.py entrypoint.sh ./
COPY scripts/ ./scripts/
RUN chmod +x entrypoint.sh scripts/download-model.sh

VOLUME /data/kws/models
EXPOSE 10400

HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD python -c "import sys,urllib.request; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:10400/health', timeout=5).status == 200 else 1)"

ENTRYPOINT ["/app/entrypoint.sh"]
CMD []
