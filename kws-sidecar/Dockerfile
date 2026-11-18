# KWS 边车：sherpa-onnx WenetSpeech 唤醒检测（服务端，NAS x86_64）
# 模型不 bake 进镜像：entrypoint 在模型缺失时下载（见 scripts/download-model.sh），
# 下载失败容器明确报错退出；模型持久化在 kws-data 卷。
FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    KWS_HOST=127.0.0.1 \
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

# S-2（2026-11-18 第三轮审计 P1）：镜像内建好模型目录并改属主
# 1000:1000——全新具名卷首次挂载时 Docker 把镜像里该目录的属主
# copy-up 进卷，uid 1000 的 entrypoint 才能写模型；只靠 compose 的
# user: "1000:1000" 不够（SC-P2-12 半修：旧镜像里根本没有这一步）。
# 注意顺序：这一步必须在下面的 VOLUME 声明之前，VOLUME 之后再改
# 目录内容会被丢弃。已存在的 root 属主旧卷不受 copy-up 影响，仍需
# nas-install.sh §4 的 chown one-shot 迁移；entrypoint 在下载前另
# 有目录可写预检（见 entrypoint.sh），写不进时给可读 FATAL。
RUN mkdir -p /data/kws/models && chown 1000:1000 /data/kws/models

VOLUME /data/kws/models
EXPOSE 10400

HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD python -c "import sys,urllib.request; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:10400/health', timeout=5).status == 200 else 1)"

ENTRYPOINT ["/app/entrypoint.sh"]
CMD []
