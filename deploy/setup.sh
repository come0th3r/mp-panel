#!/usr/bin/env bash
# MP Panel installer for Debian and Ubuntu, x86_64.
set -Eeuo pipefail
umask 077

MP_RELEASE_VERSION='v0.3.0'
REPOSITORY="${MP_REPOSITORY:-come0th3r/mp-panel}"
DESTINATION='/opt/mp-panel'
SOURCE=''
ASSUME_YES=false
CONTACT=''
PUBLIC_URL=''
AGENT_URL=''
GATEWAY=''
GAME_DOMAIN=''
WEB_BIND=''
WEB_PORT='8080'
AGENT_PORT='8443'
GATEWAY_PORT='25565'
NPM_NETWORK=''
TEMP_DIRECTORY=''

fail() { printf 'Ошибка: %s\n' "$*" >&2; exit 1; }
cleanup() {
  if [[ -n "$TEMP_DIRECTORY" && "$TEMP_DIRECTORY" == /tmp/mp-setup.* && -d "$TEMP_DIRECTORY" && ! -L "$TEMP_DIRECTORY" ]]; then
    rm -rf -- "$TEMP_DIRECTORY"
  fi
}
trap cleanup EXIT
trap 'printf "Установка прервана на строке %s. Данные Docker и настройки сохранены.\n" "$LINENO" >&2' ERR

usage() {
  cat <<'HELP'
MP Panel: установка на Debian или Ubuntu (x86_64).

  sudo bash setup.sh
  sudo bash deploy/setup.sh --source /path/to/mp-panel
  sudo bash setup.sh --yes --contact admin@example.com --public-url http://192.168.1.10:8080 --agent-url https://192.168.1.10:8443

Параметры:
  --version TAG       Версия релиза, по умолчанию v0.3.0.
  --repository NAME   Репозиторий GitHub в формате owner/name.
  --source PATH       Каталог исходников или архив .tar.gz вместо скачивания.
  --directory PATH    Каталог установки, по умолчанию /opt/mp-panel.
  --contact EMAIL     Email администратора для официального каталога Paper.
  --public-url URL    Адрес панели, доступный пользователям.
  --agent-url URL     HTTPS-адрес отдельного порта агентов.
  --web-bind IP       Адрес привязки панели, по умолчанию 0.0.0.0.
  --web-port PORT     Локальный порт панели, по умолчанию 8080.
  --agent-port PORT   Локальный порт агентов, по умолчанию 8443.
  --gateway PROVIDER  none, mc-router или gate; по умолчанию none.
  --game-domain NAME  Домен игровых серверов при включенном шлюзе.
  --gateway-port PORT Локальный TCP-порт шлюза, по умолчанию 25565.
  --npm-network NAME  Имя существующей Docker-сети Nginx Proxy Manager.
  --yes               Без вопросов; разрешает установку Docker.
  --help              Показать справку.

При обновлении существующий .env сохраняется целиком. Параметры новых
настроек к нему не применяются. Сначала собирается новая версия, затем
создаются копии базы и ключей. При сбое возвращается прежняя версия.
Действующие Compose-файлы сохраняются. Minecraft-серверы не создаются.
HELP
}

while (( $# )); do
  case "$1" in
    --help|-h) usage; exit 0 ;;
    --yes) ASSUME_YES=true; shift ;;
    --version|--repository|--source|--directory|--contact|--public-url|--agent-url|--web-bind|--web-port|--agent-port|--gateway|--game-domain|--gateway-port|--npm-network)
      (( $# >= 2 )) || fail "Для $1 нужно значение."
      case "$1" in
        --version) MP_RELEASE_VERSION=$2 ;; --repository) REPOSITORY=$2 ;;
        --source) SOURCE=$2 ;; --directory) DESTINATION=$2 ;;
        --contact) CONTACT=$2 ;; --public-url) PUBLIC_URL=$2 ;;
        --agent-url) AGENT_URL=$2 ;; --web-bind) WEB_BIND=$2 ;;
        --web-port) WEB_PORT=$2 ;; --agent-port) AGENT_PORT=$2 ;;
        --gateway) GATEWAY=$2 ;; --game-domain) GAME_DOMAIN=$2 ;;
        --gateway-port) GATEWAY_PORT=$2 ;; --npm-network) NPM_NETWORK=$2 ;;
      esac
      shift 2 ;;
    *) fail "Неизвестный параметр: $1. Используйте --help." ;;
  esac
done

[[ $EUID -eq 0 ]] || fail 'Запустите установщик через sudo bash setup.sh.'
[[ "$(uname -m)" == x86_64 ]] || fail 'Этот релиз поддерживает только x86_64 (amd64).'
[[ -r /etc/os-release ]] || fail 'Не удалось определить Linux-дистрибутив.'
# Keep operating-system metadata separate from installer settings.
MP_OS_ID=$( . /etc/os-release; printf '%s' "${ID:-}" )
[[ "$MP_OS_ID" == debian || "$MP_OS_ID" == ubuntu ]] || fail 'Поддерживаются Debian и Ubuntu.'
[[ "$REPOSITORY" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || fail 'Неверный репозиторий GitHub.'
[[ "$MP_RELEASE_VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-+][A-Za-z0-9.-]+)?$ ]] || fail 'Неверная версия релиза.'

[[ "$DESTINATION" == /* && "$DESTINATION" != *$'\n'* && "$DESTINATION" != *'//'* && "$DESTINATION" != *'/../'* && "$DESTINATION" != */.. ]] || fail 'Нужен абсолютный путь без переходов .. и повторных / .'
[[ "$DESTINATION" == / ]] || DESTINATION=${DESTINATION%/}
case "$DESTINATION" in
  /|/opt|/etc|/usr|/var|/home|/root|/tmp|/bin|/sbin|/lib|/lib64|/boot|/dev|/proc|/sys|/run|/etc/*|/usr/*|/bin/*|/sbin/*|/lib/*|/lib64/*|/boot/*|/dev/*|/proc/*|/sys/*|/run/*)
    fail 'Выберите отдельный каталог приложения, например /opt/mp-panel.' ;;
esac
# Reject symlinks in every component before writing or updating the checkout.
checked_path=''
IFS='/' read -r -a destination_parts <<< "$DESTINATION"
for component in "${destination_parts[@]}"; do
  [[ -n "$component" ]] || continue
  [[ "$component" != . ]] || fail 'В каталоге установки не допускается компонент . .'
  checked_path="$checked_path/$component"
  [[ ! -L "$checked_path" ]] || fail "Каталог установки содержит символическую ссылку: $checked_path"
done
[[ ! -e "$DESTINATION" || -d "$DESTINATION" ]] || fail 'Путь установки занят файлом.'
[[ ! -L "$DESTINATION/.env" ]] || fail '.env не должен быть символической ссылкой.'
if [[ -d "$DESTINATION" && ! -f "$DESTINATION/.env" && ! -f "$DESTINATION/compose.yaml" ]]; then
  [[ -z "$(find "$DESTINATION" -mindepth 1 -maxdepth 1 -print -quit)" ]] || fail 'Каталог уже содержит посторонние файлы. Выберите пустой каталог.'
fi

prompt() {
  local variable=$1 label=$2 default=${3:-} answer
  if $ASSUME_YES; then printf -v "$variable" '%s' "$default"; return; fi
  [[ -r /dev/tty ]] || fail 'Нет терминала для вопросов. Используйте --yes с нужными параметрами.'
  printf '%s' "$label" >/dev/tty
  [[ -z "$default" ]] || printf ' [%s]' "$default" >/dev/tty
  printf ': ' >/dev/tty
  IFS= read -r answer </dev/tty || fail 'Не удалось прочитать ответ.'
  printf -v "$variable" '%s' "${answer:-$default}"
}
valid_port() { [[ "$1" =~ ^[0-9]{1,5}$ ]] && (( 10#$1 >= 1 && 10#$1 <= 65535 )); }
valid_url() { [[ "$1" =~ ^https?://[A-Za-z0-9][A-Za-z0-9.:-]*$ ]]; }

EXISTING_ENV=false
if [[ -f "$DESTINATION/.env" ]]; then
  EXISTING_ENV=true
  printf 'Найдена установленная панель в %s. Настройки .env будут сохранены.\n' "$DESTINATION"
else
  detected_ip=$(hostname -I 2>/dev/null | awk '{for (i=1; i<=NF; i++) if ($i ~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/) {print $i; exit}}') || detected_ip=''
  detected_ip=${detected_ip:-127.0.0.1}
  [[ -n "$PUBLIC_URL" ]] || prompt PUBLIC_URL 'Адрес панели' "http://$detected_ip:$WEB_PORT"
  [[ -n "$AGENT_URL" ]] || prompt AGENT_URL 'HTTPS-адрес агентов, доступный игровым хостам' "https://$detected_ip:$AGENT_PORT"
  [[ -n "$CONTACT" ]] || prompt CONTACT 'Email администратора для каталога Paper'
  [[ -n "$WEB_BIND" ]] || WEB_BIND='0.0.0.0'
  [[ -n "$GATEWAY" ]] || prompt GATEWAY 'Игровой шлюз: none, mc-router или gate' 'none'
  if [[ "$GATEWAY" != none ]]; then
    [[ -n "$GAME_DOMAIN" ]] || prompt GAME_DOMAIN 'Домен игровых серверов, например mc.example.com'
    if ! $ASSUME_YES && [[ "$GATEWAY_PORT" == 25565 ]]; then
      prompt GATEWAY_PORT 'Локальный TCP-порт игрового шлюза' '25565'
    fi
  fi
  if ! $ASSUME_YES && [[ -z "$NPM_NETWORK" ]]; then
    prompt NPM_NETWORK 'Docker-сеть Nginx Proxy Manager, пусто если не используется'
  fi
  valid_url "$PUBLIC_URL" || fail 'Адрес панели должен иметь вид http://host:8080 или https://panel.example.com без пути.'
  valid_url "$AGENT_URL" && [[ "$AGENT_URL" == https://* ]] || fail 'Адрес агентов должен иметь вид https://host:8443 без пути.'
  [[ "$CONTACT" =~ ^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$ ]] || fail 'Укажите email администратора через --contact или в ответе на вопрос.'
  [[ "$WEB_BIND" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail 'В --web-bind нужен IPv4-адрес.'
  valid_port "$WEB_PORT" && valid_port "$AGENT_PORT" && valid_port "$GATEWAY_PORT" || fail 'Порты должны быть от 1 до 65535.'
  [[ "$WEB_PORT" != "$AGENT_PORT" ]] || fail 'Панели и агентам нужны разные локальные порты.'
  [[ "$GATEWAY" == none || "$GATEWAY" == mc-router || "$GATEWAY" == gate ]] || fail 'Шлюз должен быть none, mc-router или gate.'
  if [[ "$GATEWAY" != none ]]; then
    [[ "$GAME_DOMAIN" =~ ^[A-Za-z0-9][A-Za-z0-9.-]*\.[A-Za-z]{2,}$ && "$GAME_DOMAIN" != *..* ]] || fail 'Укажите корректный домен через --game-domain.'
    [[ "$GATEWAY_PORT" != "$WEB_PORT" && "$GATEWAY_PORT" != "$AGENT_PORT" ]] || fail 'Игровому шлюзу нужен отдельный локальный порт.'
  fi
  [[ -z "$NPM_NETWORK" || "$NPM_NETWORK" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || fail 'Неверное имя Docker-сети NPM.'
fi

printf 'Устанавливаем MP Panel %s в %s.\n' "$MP_RELEASE_VERSION" "$DESTINATION"
if ! command -v curl >/dev/null 2>&1 || ! command -v openssl >/dev/null 2>&1; then
  apt-get update
  apt-get install -y ca-certificates curl openssl
fi
TEMP_DIRECTORY=$(mktemp -d /tmp/mp-setup.XXXXXXXX)

if ! command -v docker >/dev/null 2>&1; then
  docker_answer='yes'
  prompt docker_answer 'Docker не установлен. Установить Docker Engine с официального сайта? yes/no' 'yes'
  [[ "$docker_answer" == yes || "$docker_answer" == y || "$docker_answer" == да ]] || fail 'Установите Docker Engine и Compose, затем запустите установщик снова.'
  curl --proto '=https' --tlsv1.2 -fsSL https://get.docker.com -o "$TEMP_DIRECTORY/docker-install.sh"
  sh "$TEMP_DIRECTORY/docker-install.sh"
fi
docker compose version >/dev/null 2>&1 || fail 'Нужен плагин docker compose. Установите docker-compose-plugin.'
systemctl enable --now docker
docker info >/dev/null
if docker volume inspect mp-panel_database >/dev/null 2>&1; then
  $EXISTING_ENV || fail 'Обнаружена существующая база MP Panel. Восстановите ее прежний .env перед установкой.'
fi
existing_container=$(docker ps -aq --filter label=com.docker.compose.project=mp-panel --filter label=com.docker.compose.service=api | head -n 1)
if [[ -n "$existing_container" ]]; then
  existing_directory=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project.working_dir"}}' "$existing_container")
  [[ "$existing_directory" == "$DESTINATION" ]] || fail "На этом хосте уже есть MP Panel в $existing_directory. Используйте --directory с прежним каталогом."
  $EXISTING_ENV || fail 'Для существующих томов MP Panel требуется прежний .env. Восстановите его из своей резервной копии.'
fi

SCRIPT_DIRECTORY=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
if [[ -z "$SOURCE" && -f "$SCRIPT_DIRECTORY/../compose.yaml" && -f "$SCRIPT_DIRECTORY/../go.mod" ]]; then
  SOURCE=$(cd -- "$SCRIPT_DIRECTORY/.." && pwd -P)
fi
if [[ -z "$SOURCE" ]]; then
  asset="mp-panel-$MP_RELEASE_VERSION.tar.gz"
  release_url="https://github.com/$REPOSITORY/releases/download/$MP_RELEASE_VERSION"
  printf 'Скачиваем исходники релиза %s из %s.\n' "$MP_RELEASE_VERSION" "$REPOSITORY"
  curl --proto '=https' --tlsv1.2 -fSL --retry 3 "$release_url/$asset" -o "$TEMP_DIRECTORY/$asset"
  curl --proto '=https' --tlsv1.2 -fsSL --retry 3 "$release_url/SHA256SUMS" -o "$TEMP_DIRECTORY/SHA256SUMS"
  checksum=$(awk -v name="$asset" '$2 == name || $2 == "*" name {print $1}' "$TEMP_DIRECTORY/SHA256SUMS")
  [[ "$checksum" =~ ^[a-fA-F0-9]{64}$ ]] || fail 'В SHA256SUMS нет единственной корректной суммы архива.'
  printf '%s  %s\n' "$checksum" "$asset" > "$TEMP_DIRECTORY/source.sha256"
  (cd "$TEMP_DIRECTORY" && sha256sum -c source.sha256)
  SOURCE="$TEMP_DIRECTORY/$asset"
fi

if [[ -f "$SOURCE" ]]; then
  mkdir "$TEMP_DIRECTORY/source"
  # Release archives contain regular files and directories only. Reject paths
  # that could escape extraction or replace installation secrets.
  tar -tzf "$SOURCE" > "$TEMP_DIRECTORY/archive.paths"
  tar -tvzf "$SOURCE" > "$TEMP_DIRECTORY/archive.types"
  awk 'substr($0,1,1) != "-" && substr($0,1,1) != "d" {exit 1}' "$TEMP_DIRECTORY/archive.types" || fail 'Архив содержит ссылки или специальные файлы.'
  while IFS= read -r archive_path; do
    archive_path=${archive_path#./}
    [[ -z "$archive_path" ]] && continue
    [[ "$archive_path" =~ ^[A-Za-z0-9_./@+-]+$ && "$archive_path" != /* && "$archive_path" != ../* && "$archive_path" != *'/../'* && "$archive_path" != */.. ]] || fail "Небезопасный путь в архиве: $archive_path"
    case "/$archive_path" in */.env|*/.git|*/.git/*) fail 'Архив содержит секреты или служебный Git-каталог.' ;; esac
  done < "$TEMP_DIRECTORY/archive.paths"
  tar -xzf "$SOURCE" -C "$TEMP_DIRECTORY/source" --no-same-owner --no-same-permissions
  SOURCE="$TEMP_DIRECTORY/source"
fi
[[ -d "$SOURCE" && -f "$SOURCE/compose.yaml" && -f "$SOURCE/go.mod" && -f "$SOURCE/Dockerfile" ]] || fail 'Нужен каталог или архив исходников MP Panel с compose.yaml в корне.'
SOURCE=$(cd -- "$SOURCE" && pwd -P)
# Only application source is copied. Local credentials, logs, Git files and
# working notes are never included in a local installation.
source_entries=(cmd internal apps deploy go.mod go.sum Dockerfile compose.yaml .dockerignore .env.example)
[[ ! -f "$SOURCE/README.md" ]] || source_entries+=(README.md)
if [[ -d "$SOURCE/docs" ]]; then
  source_entries+=(docs/DEPLOYMENT.md docs/GATEWAY.md docs/API_CONTRACT.md)
fi
for source_entry in "${source_entries[@]}"; do
  [[ -e "$SOURCE/$source_entry" ]] || fail "В исходниках отсутствует $source_entry."
  [[ ! -L "$SOURCE/$source_entry" ]] || fail "Исходники содержат символическую ссылку: $source_entry"
  special_entry=$(find "$SOURCE/$source_entry" \( -name node_modules -o -name .vite -o -name dist -o -name .git \) -prune -o \( ! -type f ! -type d \) -print -quit)
  [[ -z "$special_entry" ]] || fail "В $source_entry обнаружена ссылка или специальный файл: $special_entry"
done
if $EXISTING_ENV; then
  [[ -f "$SOURCE/deploy/update.sh" && ! -L "$SOURCE/deploy/update.sh" ]] || fail 'В выбранных исходниках нет безопасного установщика обновлений deploy/update.sh.'
  source "$SOURCE/deploy/update.sh"
  update_panel
  configured_url=$(sed -n 's/^MP_PUBLIC_URL=//p' "$DESTINATION/.env" | tail -n 1)
  printf 'Панель: %s\nНастройки: %s/.env\n' "$configured_url" "$DESTINATION"
  printf 'Учетные записи, агенты и игровые серверы используют прежнюю регистрацию. Агенты обновляются отдельно из панели.\n'
  exit 0
fi
mkdir -p -- "$DESTINATION"
chmod 750 "$DESTINATION"
if [[ "$SOURCE" != "$(cd -- "$DESTINATION" && pwd -P)" ]]; then
  for source_entry in "${source_entries[@]}"; do
    [[ ! -L "$DESTINATION/$source_entry" ]] || fail "В каталоге установки есть символическая ссылка: $source_entry"
    if [[ -d "$DESTINATION/$source_entry" ]]; then
      [[ -z "$(find "$DESTINATION/$source_entry" -type l -print -quit)" ]] || fail "В каталоге установки $source_entry есть символическая ссылка."
    fi
  done
  tar -C "$SOURCE" --exclude=node_modules --exclude=.vite --exclude=dist --exclude=.env --exclude=.git -cf "$TEMP_DIRECTORY/application.tar" "${source_entries[@]}"
  tar -xf "$TEMP_DIRECTORY/application.tar" -C "$DESTINATION" --no-same-owner --no-same-permissions
fi

if ! $EXISTING_ENV; then
  database_password=$(openssl rand -hex 32)
  cookie_secure=true
  [[ "$PUBLIC_URL" != http://* ]] || cookie_secure=false
  cat > "$TEMP_DIRECTORY/panel.env" <<ENV
POSTGRES_PASSWORD=$database_password
MP_PUBLIC_URL=$PUBLIC_URL
MP_AGENT_PUBLIC_URL=$AGENT_URL
MP_DOWNLOAD_USER_AGENT=MP-Panel/${MP_RELEASE_VERSION#v} ($CONTACT)
MP_COOKIE_SECURE=$cookie_secure
MP_WEB_BIND=$WEB_BIND
MP_WEB_PORT=$WEB_PORT
MP_AGENT_BIND=0.0.0.0
MP_AGENT_PORT=$AGENT_PORT
ENV
  if [[ -n "$NPM_NETWORK" ]]; then printf 'MP_NPM_NETWORK=%s\n' "$NPM_NETWORK" >> "$TEMP_DIRECTORY/panel.env"; fi
  if [[ "$GATEWAY" != none ]]; then
    printf 'MP_ENABLE_GATEWAY=true\nMP_GATEWAY_PROVIDER=%s\nMP_GAME_DOMAIN=%s\nMP_GATEWAY_BIND=0.0.0.0\nMP_GATEWAY_PORT=%s\n' "$GATEWAY" "$GAME_DOMAIN" "$GATEWAY_PORT" >> "$TEMP_DIRECTORY/panel.env"
  fi
  install -m 600 "$TEMP_DIRECTORY/panel.env" "$DESTINATION/.env"
fi
chmod 600 "$DESTINATION/.env"
cd -- "$DESTINATION"
compose=(docker compose --project-name mp-panel --env-file .env -f compose.yaml)
if grep -Eq '^MP_NPM_NETWORK=.+$' .env; then
  configured_network=$(sed -n 's/^MP_NPM_NETWORK=//p' .env | tail -n 1)
  docker network inspect "$configured_network" >/dev/null 2>&1 || fail "Не найдена Docker-сеть NPM: $configured_network. Исправьте MP_NPM_NETWORK в $DESTINATION/.env."
  compose+=(-f deploy/compose.npm.yaml)
fi
if grep -Eq '^MP_ENABLE_GATEWAY=true[[:space:]]*$' .env; then
  if grep -Eq '^MP_GATEWAY_PROVIDER=mc-router[[:space:]]*$' .env; then
    compose+=(-f deploy/compose.mc-router.yaml)
  else
    compose+=(-f deploy/compose.gate.yaml)
  fi
fi
"${compose[@]}" config --quiet
"${compose[@]}" up -d --build --wait --wait-timeout 240
configured_url=$(sed -n 's/^MP_PUBLIC_URL=//p' .env | tail -n 1)
printf '\nПанель установлена: %s\nНастройки: %s/.env\n' "$configured_url" "$DESTINATION"
owner_count=$("${compose[@]}" exec -T postgres psql -U mp -d mp -Atqc 'SELECT count(*) FROM mp_users')
if [[ "$owner_count" == 0 ]]; then
  setup_token=$("${compose[@]}" exec -T api cat /var/lib/mp/keys/setup.token)
  printf 'Токен первого владельца: %s\nОткройте панель и создайте учетную запись владельца.\n' "$setup_token"
elif [[ "$owner_count" =~ ^[0-9]+$ ]]; then
  printf 'Учетная запись владельца уже создана. Используйте прежний логин.\n'
else
  fail 'Не удалось определить состояние учетной записи владельца.'
fi
printf 'Игровые хосты добавляются в панели по SSH. Порты NAT, DNS и HTTPS-прокси настраиваются отдельно.\n'
if grep -Eq '^MP_ENABLE_GATEWAY=true[[:space:]]*$' .env; then
  printf 'Для игровых доменов направьте DNS на адрес шлюза. Внешний TCP 25565 должен вести на его локальный порт из .env.\n'
fi
