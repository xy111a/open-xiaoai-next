# KWS 边车：sherpa-onnx WenetSpeech 唤醒检测（服务端，NAS x86_64）
# 模型不 bake 进镜像：entrypoint 在模型缺失时下载（见 scripts/download-model.sh），
# 下载失败容器明确报错退出；模型持久化在 kws-data 卷。
#
# S-7（2026-11-18 第三轮审计 P2）：基础镜像按 P1-4 同样做法钉 digest。
# 钉的值：python:3.12-slim@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b
# 查询方法（2026-11-18）：Docker Hub API
#   curl -s https://hub.docker.com/v2/namespaces/library/repositories/python/tags/3.12-slim
# 取返回 JSON 的顶层 `digest` 字段（manifest list digest，2026-11-08 更新）；
# 该 digest 覆盖多架构，x86_64（NAS）对应镜像 digest 为
# sha256:05a994d2344eb364cc186910f4fc8786a38215f39f00d0acc25bff33a08f4177。
# 更新流程：定期（或重编镜像前）重查上述 API，比对顶层 digest 是否变化；
# 变化时更新本行 digest，同时核对 apt 包版本（见下）与 requirements.txt，
# 在 NAS 上重新构建并跑 `docker compose up -d kws-sidecar` + /health 冒烟；
# 不要只改 tag 不改 digest——digest 不变时构建结果不变，漂移即被钉住。
# 构建验证：本机无 docker，无法本地构建验证——Dockerfile 已人工逐行
# 核对语法，构建验证待 NAS 实测。
FROM python:3.12-slim@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b

ENV PYTHONUNBUFFERED=1 \
    KWS_HOST=127.0.0.1 \
    KWS_PORT=10400 \
    KWS_MODEL_DIR=/data/kws/models

# S-7：apt 包同样钉版本（Debian bookworm，2026-11-18 查
# deb.debian.org bookworm Packages：curl 7.88.1-10+deb12u15、
# bzip2 1.0.8-5+b1）。基础镜像更新到新 bookworm 点版本时 apt 版本
# 可能前移，构建失败即提示更新此处版本（fail loudly，不静默漂移）。
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        curl=7.88.1-10+deb12u15 bzip2=1.0.8-5+b1 \
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

# S-7：镜像内以非 root 运行（与 deploy/docker-compose.yml 的
# user: "1000:1000" 一致；独立 `docker run` 也不跑 root）。
# 位置注意：必须在上方 mkdir + chown（root 才能做）之后、ENTRYPOINT
# 之前；entrypoint 只写属主 1000:1000 的模型目录、不需要 root。
USER 1000:1000

ENTRYPOINT ["/app/entrypoint.sh"]
CMD []
