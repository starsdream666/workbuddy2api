#!/usr/bin/env bash
# login.sh — WorkBuddy OAuth 登录 → 落盘 auth 文件（按产品线 realm）
#
# 用法:
#   ./login.sh                # 默认 cn：CodeBuddy CN（copilot.tencent.com / www.codebuddy.cn）
#   ./login.sh workbuddy      # WorkBuddy AI（www.workbuddy.ai，桌面端那条线；旧名 ai 等价）
#   ./login.sh codebuddy      # CodeBuddy IDE 域（www.codebuddy.ai，workbuddy 的路线别名）
#
# 流程:
#   1. POST /v2/plugin/auth/state 拿授权 URL（无 PKCE，state 由服务端签发）
#   2. 你在浏览器打开 URL 完成登录
#   3. 回到这里按 y → poll 拿 token+uid+nickname → 签到 → 落盘 auth 文件
#      （cn: auths/workbuddy-<uid>.json；workbuddy/codebuddy: auths/workbuddy-ai-<uid>.json，含 realm 字段）
#   4. 重启 workbuddy2api 容器加载新账号
#
# 提示：若本机已装 WorkBuddy 桌面端并登录过，可跳过本脚本，直接
#   go run ./cmd/importauth
# 复用桌面端凭证（见 README「WorkBuddy AI 线」）。
set -euo pipefail

cd "$(dirname "$0")"
AUTH_DIR="./auths"
CONTAINER="workbuddy2api"

# 产品线（realm）：cn（默认）/ workbuddy / codebuddy；旧名 ai 仍然接受（等价 workbuddy）。
# codebuddy 是 workbuddy 的**路线别名**（同一后端、同一账号空间）：它的凭证按来源线 workbuddy 归档，
# 因此这里 FILE_PREFIX 与写入的 realm 都取 workbuddy（绝不会另起一个 codebuddy 池）。
REALM="${1:-cn}"
case "$REALM" in
    cn)           REALM_LABEL="CodeBuddy CN";  BILLING_BASE="https://www.codebuddy.cn";   FILE_PREFIX="workbuddy";    AUTH_REALM="cn" ;;
    workbuddy|ai) REALM_LABEL="WorkBuddy AI";  BILLING_BASE="https://www.workbuddy.ai"; FILE_PREFIX="workbuddy-ai"; AUTH_REALM="workbuddy" ;;
    codebuddy)    REALM_LABEL="CodeBuddy IDE"; BILLING_BASE="https://www.codebuddy.ai"; FILE_PREFIX="workbuddy-ai"; AUTH_REALM="workbuddy" ;;
    *)            echo "未知 realm: $REALM（可选 cn / workbuddy / codebuddy；旧名 ai 等价 workbuddy）" >&2; exit 1 ;;
esac

mkdir -p "$AUTH_DIR"

# login 工具：优先用现成的；容器内镜像已预编译为 /app/login（无 Go 源码，不能现场编译）
LOGIN_BIN="./login"
if [[ ! -x "$LOGIN_BIN" && -x "/app/login" ]]; then
    LOGIN_BIN="/app/login"
fi
if [[ ! -x "$LOGIN_BIN" ]]; then
    go build -o "$LOGIN_BIN" ./cmd/login
fi

echo "============================================================"
echo "  WorkBuddy OAuth 登录（realm: $REALM — $REALM_LABEL）"
echo "============================================================"
echo ""

AUTH_URL=$("$LOGIN_BIN" url -realm "$REALM")

echo "请在浏览器中打开以下链接完成登录："
echo ""
echo "  $AUTH_URL"
echo ""

if command -v xclip &>/dev/null; then
    echo -n "$AUTH_URL" | xclip -selection clipboard 2>/dev/null && echo "(已复制到剪贴板)"
elif command -v xsel &>/dev/null; then
    echo -n "$AUTH_URL" | xsel --clipboard 2>/dev/null && echo "(已复制到剪贴板)"
fi

echo ""
read -rp "完成登录后按 y 继续: " ans
if [[ "$ans" != "y" && "$ans" != "Y" ]]; then
    echo "已取消"
    exit 1
fi

echo ""
echo "正在获取 token..."

RESULT=$("$LOGIN_BIN" poll -realm "$REALM") || {
    echo ""
    echo "获取 token 失败。可能原因："
    echo "  - 登录还没完成就按了 y（重新运行 ./login.sh 再试）"
    echo "  - 登录页报错（把报错截图发出来排查）"
    exit 1
}

TOKEN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['access_token'])")
REFRESH=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['refresh_token'])")
EXPIRES_IN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['expires_in'])")
DOMAIN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('domain',''))")
USER_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('uid',''))")
ENT_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('enterprise_id',''))")
NICKNAME=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('nickname',''))")

if [[ -z "$USER_ID" ]]; then
    echo "无法获取 uid，请检查 token 是否有效"
    exit 1
fi

EXPIRES_AT=$(( $(date +%s) + EXPIRES_IN ))

# ─── 签到（POST <billing_base>/v2/billing/meter/daily-checkin，幂等不阻塞）───
python3 - <<PYEOF
import json, urllib.request, urllib.error

req = urllib.request.Request(
    "$BILLING_BASE/v2/billing/meter/daily-checkin",
    method="POST", data=b"{}",
    headers={
        "Authorization": "Bearer $TOKEN",
        "Accept": "application/json",
        "Content-Type": "application/json",
        "X-User-Id": "$USER_ID",
        **({"X-Enterprise-Id": "$ENT_ID", "X-Tenant-Id": "$ENT_ID"} if "$ENT_ID" else {}),
        **({"X-Domain": "$DOMAIN"} if "$DOMAIN" else {}),
    })
try:
    with urllib.request.urlopen(req, timeout=15) as r:
        body = json.loads(r.read().decode() or "{}")
    if body.get("code") == 0:
        data = body.get("data") or {}
        print(f"签到: 成功 {json.dumps(data, ensure_ascii=False)[:150]}")
    else:
        print(f"签到: {body.get('msg', json.dumps(body)[:150])}")
except urllib.error.HTTPError as e:
    # 已签到等业务错误也走 4xx（实测 code=10001 "今天已签到"）
    try:
        body = json.loads(e.read().decode() or "{}")
        print(f"签到: {body.get('msg', 'http %d' % e.code)}")
    except Exception:
        print(f"签到: http {e.code}")
except Exception as e:
    print(f"签到: {e}")
PYEOF

# ─── 落盘 auth 文件（与 internal/auth 读取格式一致）─────────────────
AUTH_FILE="$AUTH_DIR/${FILE_PREFIX}-${USER_ID}.json"
if [[ -f "$AUTH_FILE" ]]; then
    echo "账号已存在（uid=${USER_ID}），将覆盖更新凭证"
    ACTION="覆盖"
else
    echo "新账号（uid=${USER_ID}），新增 auth 文件"
    ACTION="新增"
fi
python3 - <<PYEOF
import json

auth = {
    "realm": "$AUTH_REALM",
    "account": {
        "uid": "$USER_ID",
        "enterpriseId": "$ENT_ID",
        "nickname": "$NICKNAME"
    },
    "auth": {
        "accessToken": "$TOKEN",
        "refreshToken": "$REFRESH",
        "expiresAt": $EXPIRES_AT,
        "domain": "$DOMAIN"
    }
}
with open("$AUTH_FILE", "w") as f:
    json.dump(auth, f, indent=1)
print(f"已保存（${ACTION}）: $AUTH_FILE")
PYEOF

# ─── 重启服务（容器内运行时跳过：容器内没有 docker CLI）──────────
echo ""
if [[ -f /.dockerenv ]]; then
    # 容器内登录：auth 文件已写入挂载卷（/app/auths），账号池需重启才重新加载目录
    echo "检测到容器内运行：auth 文件已写入 ${AUTH_DIR}（挂载卷）。"
    echo "请在宿主机执行以下命令让账号池加载新账号："
    echo "  docker compose restart ${CONTAINER}"
elif docker ps --format '{{.Names}}' | grep -q "^${CONTAINER}$"; then
    echo "重启 $CONTAINER 加载新账号..."
    docker restart "$CONTAINER" >/dev/null
    sleep 2
    # API_KEY 从 config.json 读取（该变量在脚本中未定义，fallback 仅为占位，不会通过鉴权）
    API_KEY=$(python3 -c "import json; print(json.load(open('config.json')).get('api_key',''))" 2>/dev/null)
    COUNT=$(curl -s http://127.0.0.1:7863/status -H "Authorization: Bearer ${API_KEY:-test_key}" 2>/dev/null | python3 -c "import json,sys; print(len(json.load(sys.stdin).get('accounts',[])))" 2>/dev/null || echo "?")
    echo "服务已重启，当前账号数: $COUNT"
else
    echo "容器 $CONTAINER 未运行，auth 文件已保存，下次启动自动加载"
fi

echo ""
echo "============================================================"
echo "  登录完成！"
echo "  UID: $USER_ID"
echo "  Nickname: ${NICKNAME:-（未获取到）}"
echo "  Realm: $REALM → auth 文件按来源线 $AUTH_REALM 归档（${FILE_PREFIX}-${USER_ID}.json）"
echo "  Token: ${TOKEN:0:30}..."
echo "  有效期: $(date -d "@$EXPIRES_AT" '+%Y-%m-%d %H:%M' 2>/dev/null || echo "$EXPIRES_AT")"
echo "============================================================"
