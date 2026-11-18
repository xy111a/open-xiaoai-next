#!/usr/bin/env bash
# gen-certs.sh — 生成 open-xiaoai-next 的 mTLS 证书材料（P0-3）。
#
# 产物（OUT_DIR，默认 firmware-patch/work/certs）：
#   ca.crt / ca.key        自签 CA（10 年；私钥 0600，永不离开 NAS）
#   hub.crt / hub.key      CA 签发的 Hub 服务端证书（SAN: localhost,
#                          open-xiaoai-hub, 127.0.0.1；serverAuth）
#   client.crt / client.key 共享客户端证书（CN open-xiaoai-client，
#                          clientAuth；两台音箱共用，要每台独立证书时
#                          改 CN 重签，或调 tlscert.IssueClientCert）
#   F-8：共用身份的含义——两台音箱是同一个 mTLS 身份，吊销/轮换没有
#   隔离：一台的证书被吊销（或 --force 重建）会同时影响另一台；Hub
#   也无法只禁其中一台。家用 + 局域网前提下接受这一点；如需按设备
#   吊销，再按上面说的办法签发独立 client 证书（需同时改 patch.sh 的
#   证书选取与 Hub 的客户端身份映射，并重刷两台镜像，属较大改动）。
#   fingerprints.txt       各证书的 SHA-256 指纹（公开信息，可贴进
#                          client.toml 的 hub_ca_fingerprint）
#
# 用法：
#   ./scripts/gen-certs.sh [OUT_DIR]        # 已存在文件时拒绝覆盖
#   ./scripts/gen-certs.sh --force [OUT_DIR] # 轮换：删旧重建（旧证书全失效）
#
# 下游：
#   Hub 端：把 ca.crt/hub.crt/hub.key 拷到 Hub 数据目录 tls/ 下，
#           启动改调 tlscert.LoadOrCreateMTLS() + SetClientCAs()
#           （main.go 接线，见 tlscert.go 的 LoadOrCreateMTLS 注释）。
#           也可直接用 Go 的 LoadOrCreateMTLS() 生成同一套材料。
#   音箱端：打补丁时带 --cert-dir OUT_DIR（patch_firmware.py 会把
#           ca.crt/client.crt/client.key 装进镜像的 /etc/open-xiaoai/tls/）。
# 注意：OUT_DIR 默认在 firmware-patch/work/ 下（.gitignore 已排除），
# 私钥不要提交进仓库、不要发给任何人。
set -euo pipefail

FORCE=0
if [ "${1:-}" = "--force" ]; then
  FORCE=1
  shift
fi

HERE="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${1:-$HERE/work/certs}"
DAYS=3650  # 10 年

command -v openssl >/dev/null || { echo "缺 openssl" >&2; exit 1; }

if [ "$FORCE" -eq 0 ]; then
  for f in ca.crt ca.key hub.crt hub.key client.crt client.key; do
    if [ -e "$OUT/$f" ]; then
      echo "已存在 $OUT/$f：拒绝覆盖（轮换请加 --force，旧证书将全部失效）" >&2
      exit 1
    fi
  done
else
  rm -rf "$OUT"
fi
mkdir -p "$OUT"
umask 077

# --- CA：自签，CA:TRUE ---
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$OUT/ca.key" 2>/dev/null
openssl req -new -x509 -key "$OUT/ca.key" -out "$OUT/ca.crt" -days "$DAYS" \
  -subj "/CN=open-xiaoai-ca/O=open-xiaoai-next" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign,digitalSignature" 2>/dev/null

# --- Hub 服务端证书：CA 签发 ---
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$OUT/hub.key" 2>/dev/null
openssl req -new -key "$OUT/hub.key" -out "$OUT/hub.csr" \
  -subj "/CN=open-xiaoai-hub/O=open-xiaoai-next" \
  -addext "subjectAltName=DNS:localhost,DNS:open-xiaoai-hub,IP:127.0.0.1" 2>/dev/null
openssl x509 -req -in "$OUT/hub.csr" -CA "$OUT/ca.crt" -CAkey "$OUT/ca.key" \
  -CAcreateserial -out "$OUT/hub.crt" -days "$DAYS" \
  -extfile <(printf "subjectAltName=DNS:localhost,DNS:open-xiaoai-hub,IP:127.0.0.1\nextendedKeyUsage=serverAuth\nkeyUsage=digitalSignature,keyEncipherment") 2>/dev/null

# --- 客户端证书：CA 签发，CN open-xiaoai-client ---
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$OUT/client.key" 2>/dev/null
openssl req -new -key "$OUT/client.key" -out "$OUT/client.csr" \
  -subj "/CN=open-xiaoai-client/O=open-xiaoai-next" 2>/dev/null
openssl x509 -req -in "$OUT/client.csr" -CA "$OUT/ca.crt" -CAkey "$OUT/ca.key" \
  -CAcreateserial -out "$OUT/client.crt" -days "$DAYS" \
  -extfile <(printf "extendedKeyUsage=clientAuth\nkeyUsage=digitalSignature") 2>/dev/null

# --- 校验链 ---
openssl verify -CAfile "$OUT/ca.crt" "$OUT/hub.crt" >/dev/null
openssl verify -CAfile "$OUT/ca.crt" "$OUT/client.crt" >/dev/null

# --- 权限与清理 ---
chmod 600 "$OUT/ca.key" "$OUT/hub.key" "$OUT/client.key"
chmod 644 "$OUT/ca.crt" "$OUT/hub.crt" "$OUT/client.crt"
rm -f "$OUT/hub.csr" "$OUT/client.csr" "$OUT/ca.srl" "$OUT/hub.srl" "$OUT/client.srl"

# --- 指纹（公开信息） ---
{
  echo "# open-xiaoai-next mTLS 证书指纹（SHA-256，生成于 $(date -u +%Y-%m-%dT%H:%M:%SZ)）"
  for name in ca hub client; do
    echo "$name: $(openssl x509 -in "$OUT/$name.crt" -noout -fingerprint -sha256 | cut -d= -f2 | tr -d : | tr 'A-F' 'a-f')"
  done
  echo "# client.toml 用 hub 的指纹：hub_ca_fingerprint = \"<上面 hub: 的值>\""
} > "$OUT/fingerprints.txt"
chmod 644 "$OUT/fingerprints.txt"  # 指纹是公开信息

echo "证书已生成：$OUT"
echo "  CA:     ca.crt / ca.key"
echo "  Hub:    hub.crt / hub.key"
echo "  Client: client.crt / client.key"
echo "  指纹见 $OUT/fingerprints.txt（client.toml 的 hub_ca_fingerprint 用 hub 的值）"
echo "下一步：打补丁时加 --cert-dir $OUT；Hub 端把 ca.crt/hub.crt/hub.key 放进数据目录 tls/ 并启用 mTLS。"
