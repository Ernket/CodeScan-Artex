#!/usr/bin/env bash
# =============================================================================
# SCA Audit 账号密码重置脚本
#
# 多用户版本后，账号存放在 users 表（username + bcrypt password_hash），不再是
# settings 表里的单一 auth.password_hash。本脚本连上数据库后，用 pgcrypto 在库内
# 生成 bcrypt 哈希并写回指定账号——与后端登录校验（golang.org/x/crypto/bcrypt）
# 完全兼容。默认重置管理员账号 admin（旧版单账号迁移后的用户名）。
#
# 连接方式：宿主机直接用 psql 连数据库。连接信息按以下优先级获取：
#   命令行参数 > --dsn/$ARTEX_PG_DSN > config.json 的 database.*
#
# 用法示例：
#   ./reset-password.sh                          # 重置 admin，交互输入新密码
#   ./reset-password.sh -u someone -p 'NewPass!' # 重置指定账号
#   ./reset-password.sh --dsn postgres://u:p@h:5432/artex
#   ./reset-password.sh -H 127.0.0.1 -P 5433 -U artex -W pass -d artex
#
# 注意：目标库必须已经被新版本后端启动过一次（schema 会建出 users 表）。
# 安全：新密码经环境变量 + psql \getenv 传入（不进入进程 argv），并用 :'var'
# 自动转义（防 SQL 注入）；数据库密码经 PGPASSWORD 传递，同样不进 argv。
# =============================================================================
set -euo pipefail

BCRYPT_COST=10
TARGET_USER="${ARTEX_RESET_USER:-admin}"   # 要重置的账号名（-u/--target-user 覆盖）

DSN=""
HOST="" PORT="" USER="" DBPASS="" DBNAME="" SSLMODE=""
CONFIG=""
NEWPASS=""
ASSUME_YES=0

die() { echo "错误：$*" >&2; exit 1; }
info() { echo "· $*" >&2; }

usage() { awk 'NR>1 && /^#/ {sub(/^# ?/,""); print; next} NR>1 {exit}' "$0"; exit 0; }

# ---- 参数解析 -------------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dsn)            DSN="${2:-}"; shift 2 ;;
    -H|--host)        HOST="${2:-}"; shift 2 ;;
    -P|--port)        PORT="${2:-}"; shift 2 ;;
    -U|--user)        USER="${2:-}"; shift 2 ;;
    -W|--db-password) DBPASS="${2:-}"; shift 2 ;;
    -d|--dbname)      DBNAME="${2:-}"; shift 2 ;;
    -u|--target-user) TARGET_USER="${2:-}"; shift 2 ;;
    --sslmode)        SSLMODE="${2:-}"; shift 2 ;;
    --config)         CONFIG="${2:-}"; shift 2 ;;
    -p|--new-password) NEWPASS="${2:-}"; shift 2 ;;
    -y|--yes)         ASSUME_YES=1; shift ;;
    -h|--help)        usage ;;
    *) die "未知参数：$1（-h 查看用法）" ;;
  esac
done

# ---- 从 config.json 读取 database.*（仅 local 模式、且未显式给出连接时）-----
# 优先用 python3 解析（健壮）；缺 python3 时退回 grep（config.json 为规整分字段）。
read_config_json() {
  local path="$1"
  [[ -f "$path" ]] || return 1
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$path" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1])).get("database", {})
except Exception:
    sys.exit(1)
# 支持直接给 dsn，或分字段
if d.get("dsn"):
    print("DSN\t" + d["dsn"]); sys.exit(0)
for k in ("host","port","user","password","dbname","sslmode"):
    if d.get(k) is not None:
        print(k.upper() + "\t" + str(d[k]))
PY
  else
    # 极简后备：逐键 grep（值为字符串或数字）
    local k
    for k in host port user password dbname sslmode; do
      local v
      v=$(grep -oE "\"$k\"[[:space:]]*:[[:space:]]*(\"[^\"]*\"|[0-9]+)" "$path" 2>/dev/null \
            | head -1 | sed -E "s/.*:[[:space:]]*//; s/^\"//; s/\"$//") || true
      [[ -n "$v" ]] && echo -e "${k^^}\t$v"
    done
  fi
}

apply_config_fields() {
  local line key val
  while IFS=$'\t' read -r key val; do
    [[ -z "$key" ]] && continue
    case "$key" in
      DSN)      [[ -z "$DSN" ]] && DSN="$val" ;;
      HOST)     [[ -z "$HOST" ]] && HOST="$val" ;;
      PORT)     [[ -z "$PORT" ]] && PORT="$val" ;;
      USER)     [[ -z "$USER" ]] && USER="$val" ;;
      PASSWORD) [[ -z "$DBPASS" ]] && DBPASS="$val" ;;
      DBNAME)   [[ -z "$DBNAME" ]] && DBNAME="$val" ;;
      SSLMODE)  [[ -z "$SSLMODE" ]] && SSLMODE="$val" ;;
    esac
  done
}

# ---- 采集新密码 -----------------------------------------------------------
if [[ -z "$NEWPASS" ]]; then
  read -r -s -p "输入账号 $TARGET_USER 的新密码：" NEWPASS; echo >&2
  [[ -n "$NEWPASS" ]] || die "密码不能为空"
  read -r -s -p "再次输入以确认：" NEWPASS2; echo >&2
  [[ "$NEWPASS" == "$NEWPASS2" ]] || die "两次输入不一致"
fi
[[ -n "$NEWPASS" ]] || die "密码不能为空"
# 与后端 validatePassword 一致：至少 8 位、不超过 72 字节（bcrypt 硬上限）。
if [[ ${#NEWPASS} -lt 8 ]]; then die "密码长度至少 8 位"; fi
if [[ $(printf '%s' "$NEWPASS" | wc -c) -gt 72 ]]; then die "密码长度不能超过 72 字节"; fi

# 通过环境变量把密码/账号交给 psql（\getenv 读取，不进入 argv/ps）
export ARTEX_RESET_NEWPASS="$NEWPASS"
export ARTEX_RESET_TARGET="$TARGET_USER"

# 库内生成 bcrypt 并写回 users.password_hash；密码/账号用 :'var' 自动转义。
# 账号不存在时明确报错（0 行更新），避免"以为改了其实没改"。
SQL=$(cat <<SQL
\\set ON_ERROR_STOP on
\\getenv newpw ARTEX_RESET_NEWPASS
\\getenv uname ARTEX_RESET_TARGET
CREATE EXTENSION IF NOT EXISTS pgcrypto;
WITH updated AS (
  UPDATE users
     SET password_hash = crypt(:'newpw', gen_salt('bf', $BCRYPT_COST)),
         updated_at = now()
   WHERE lower(username) = lower(:'uname')
  RETURNING username
)
SELECT CASE WHEN count(*) = 0
            THEN (SELECT 1/0)  -- 触发 ON_ERROR_STOP：账号不存在
            ELSE count(*) END AS updated_users
  FROM updated;
SQL
)

# ---- 执行 -----------------------------------------------------------------
# 连接信息优先级：命令行 > --dsn/$ARTEX_PG_DSN > config.json
if [[ -z "$DSN" && -z "$HOST$USER$DBNAME" ]]; then
  [[ -n "${ARTEX_PG_DSN:-}" ]] && DSN="$ARTEX_PG_DSN"
fi
if [[ -z "$DSN" && -z "$HOST$USER$DBNAME" ]]; then
  cfg="${CONFIG:-config.json}"
  if [[ -f "$cfg" ]]; then
    info "从 $cfg 读取数据库配置"
    apply_config_fields < <(read_config_json "$cfg")
  fi
fi

command -v psql >/dev/null 2>&1 || die "本机未找到 psql（请安装 postgresql-client）"

declare -a PSQL_ARGS=()
if [[ -n "$DSN" ]]; then
  PSQL_ARGS=("$DSN")
  target="$DSN"
else
  [[ -n "$USER"   ]] || die "缺少数据库用户（-U）或有效的 config.json/DSN"
  [[ -n "$DBNAME" ]] || die "缺少数据库名（-d）或有效的 config.json/DSN"
  HOST="${HOST:-127.0.0.1}"; PORT="${PORT:-5432}"; SSLMODE="${SSLMODE:-disable}"
  PSQL_ARGS=(-h "$HOST" -p "$PORT" -U "$USER" -d "$DBNAME")
  [[ -n "$SSLMODE" ]] && export PGSSLMODE="$SSLMODE"
  [[ -n "$DBPASS" ]] && export PGPASSWORD="$DBPASS"
  target="$USER@$HOST:$PORT/$DBNAME"
fi

info "目标数据库：$target"
if [[ "$ASSUME_YES" -ne 1 ]]; then
  read -r -p "确认在该库重置 SCA Audit 密码？[y/N] " ans
  [[ "$ans" == "y" || "$ans" == "Y" ]] || die "已取消"
fi

if ! printf '%s\n' "$SQL" | psql "${PSQL_ARGS[@]}" -v ON_ERROR_STOP=1 -q >/dev/null; then
  die "写入失败。若报 pgcrypto 权限/缺失，请用具备建扩展权限的角色，或先手动执行 CREATE EXTENSION pgcrypto。"
fi

unset ARTEX_RESET_NEWPASS ARTEX_RESET_TARGET
echo "✓ 已重置账号 $TARGET_USER 的密码。请用该账号 + 新密码登录（无需重启服务）。"
