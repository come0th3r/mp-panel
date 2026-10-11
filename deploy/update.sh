#!/usr/bin/env bash
# Loaded from the verified source bundle by setup.sh. A protected copy also
# serves as the recovery entry point; do not source untrusted state files.

update_error() { printf 'Ошибка обновления: %s\n' "$*" >&2; return 1; }

update_guard_directory() {
  local checked='' component target=$1
  [[ "$target" == /* && "$target" != / && "$target" != *'/../'* && "$target" != */.. && "$target" != *'/./'* && "$target" != *'//'* ]] || return 1
  IFS='/' read -r -a update_parts <<< "$target"
  for component in "${update_parts[@]}"; do
    [[ -n "$component" ]] || continue
    checked="$checked/$component"
    [[ ! -L "$checked" ]] || return 1
  done
  [[ ! -e "$target" || -d "$target" ]]
}

update_phase() {
  printf '%s\n' "$1" > "$UPDATE_TRANSACTION/phase.new"
  chmod 600 "$UPDATE_TRANSACTION/phase.new"
  mv -f -- "$UPDATE_TRANSACTION/phase.new" "$UPDATE_TRANSACTION/phase"
  sync -f "$UPDATE_TRANSACTION/phase"
}

update_state() {
  declare -p DESTINATION PROJECT UPDATE_TRANSACTION OLD_FILES OLD_RUNNING OLD_API_IMAGE OLD_WORKER_IMAGE POSTGRES_CONTAINER PANEL_VOLUME DB_NAME DB_USER > "$UPDATE_TRANSACTION/state.new"
  chmod 600 "$UPDATE_TRANSACTION/state.new"
  mv -f -- "$UPDATE_TRANSACTION/state.new" "$UPDATE_TRANSACTION/state"
  sync -f "$UPDATE_TRANSACTION/state"
}

update_compose() {
  local -a args=(docker compose --project-name "$PROJECT" --project-directory "$DESTINATION" --env-file "$DESTINATION/.env")
  local file
  for file in "${OLD_FILES[@]}"; do args+=(-f "$file"); done
  [[ ! -f "$DESTINATION/.mp-runtime-images.json" ]] || args+=(-f "$DESTINATION/.mp-runtime-images.json")
  "${args[@]}" "$@"
}

update_image_override() {
  python3 - "$1" "$2" "$3" "$4" <<'PY'
import json,os,sys
file,api,worker,transaction=sys.argv[1:]
data={'services':{name:{'image':image,'labels':{'mp.update.id':transaction}} for name,image in [('api',api),('worker',worker)]}}
with open(file+'.new','w',encoding='utf-8') as out:json.dump(data,out)
os.chmod(file+'.new',0o600)
os.replace(file+'.new',file)
PY
}

update_check_jobs() {
  local active
  active=$(docker exec "$POSTGRES_CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -Atqc "SELECT (SELECT count(*) FROM mp_commands WHERE mutating AND state IN ('pending','delivered') AND deadline>now())+(SELECT count(*) FROM mp_work WHERE state IN ('queued','running') AND expires_at>now())") || return 1
  [[ "$active" =~ ^[0-9]+$ ]] || return 1
  [[ "$active" == 0 ]] || update_error "Есть незавершённые управляющие задания ($active). Дождитесь их завершения и повторите обновление."
}

update_verify_stopped() {
  local service container state containers
  for service in api worker; do
    containers=$(docker ps -aq --filter "label=com.docker.compose.project=$PROJECT" --filter "label=com.docker.compose.service=$service") || return 1
    while IFS= read -r container; do
      [[ -n "$container" ]] || continue
      state=$(docker inspect --format '{{.State.Running}}:{{.State.Paused}}:{{.State.Restarting}}' "$container") || return 1
      [[ "$state" == false:false:false ]] || { update_error 'API/worker ещё работают. Копирование или восстановление данных не выполняется.'; return 1; }
    done <<< "$containers"
  done
}

update_copy_source() {
  python3 - "$1" "$2" "$3" "$UPDATE_TRANSACTION" "${OLD_FILES[@]}" <<'PY'
import json,os,shutil,stat,sys
from pathlib import Path,PurePosixPath
action,source,destination,transaction,*compose=sys.argv[1:]
src,dst,txn=map(Path,[source,destination,transaction])
preserved={'compose.yaml','.env','.mp-runtime-images.json'}
installation=Path(os.environ['MP_UPDATE_DESTINATION'])
for value in compose:
 try:preserved.add(Path(value).relative_to(installation).as_posix())
 except ValueError:pass
preserved.update({'deploy/compose.npm.yaml','deploy/compose.gate.yaml','deploy/compose.mc-router.yaml','deploy/gate/config.yml'})
def safe(name):
 p=PurePosixPath(name)
 if p.is_absolute() or '..' in p.parts or not p.parts or (name.startswith('.env') and name!='.env.example') or name=='.mp-runtime-images.json':raise ValueError('unsafe managed source path')
 if p.parts[0] not in {'cmd','internal','apps','deploy','docs'} and name not in {'go.mod','go.sum','Dockerfile','.dockerignore','.env.example','README.md'}:raise ValueError('unexpected managed source path')
 return name not in preserved
def manifest(root):
 file=root/'.mp-source-files.json'
 if file.is_file():
  names=json.loads(file.read_text())
  if not isinstance(names,list) or not all(isinstance(n,str) for n in names):raise ValueError('invalid source manifest')
  return {n for n in names if safe(n)}
 # Older releases did not record a manifest. Source trees are application
 # code; configurations and files outside them remain in place.
 names=set()
 for prefix in ['cmd','internal','apps/web/src']:
  folder=root/prefix
  if folder.is_dir():names.update(p.relative_to(root).as_posix() for p in folder.rglob('*') if p.is_file())
 for name in ['go.mod','go.sum','Dockerfile','.dockerignore','.env.example','README.md','apps/web/package.json','apps/web/package-lock.json','apps/web/index.html','apps/web/vite.config.ts','apps/web/tsconfig.json','apps/web/tsconfig.node.json','deploy/setup.sh','deploy/setup.ps1','deploy/install.sh','deploy/deploy.ps1','deploy/update.sh','deploy/agent.service','docs/DEPLOYMENT.md','docs/GATEWAY.md','docs/API_CONTRACT.md']:
  if (root/name).is_file():names.add(name)
 return {n for n in names if safe(n)}
def new_manifest(root):
 names=set()
 for prefix in ['cmd','internal','apps','deploy']:
  folder=root/prefix
  for current,dirs,files in os.walk(folder,followlinks=False):
   dirs[:]=[d for d in dirs if d not in {'node_modules','dist','.vite','.git'}]
   for item in files:
    p=Path(current)/item
    if p.is_symlink() or not p.is_file() or p.stat().st_nlink!=1:raise ValueError('links and special source files are forbidden')
    n=p.relative_to(root).as_posix()
    if safe(n):names.add(n)
 for name in ['go.mod','go.sum','Dockerfile','.dockerignore','.env.example','README.md','docs/DEPLOYMENT.md','docs/GATEWAY.md','docs/API_CONTRACT.md']:
  if (root/name).is_file() and safe(name):names.add(name)
 return names
if action=='candidate':
 old=manifest(dst);new=new_manifest(src)
 (txn/'old-source-files.json').write_text(json.dumps(sorted(old)))
 (txn/'new-source-files.json').write_text(json.dumps(sorted(new)))
 remove,copy=old,new
elif action=='apply':
 remove=set(json.loads((txn/'old-source-files.json').read_text()));copy=set(json.loads((txn/'new-source-files.json').read_text()))
elif action=='restore':
 remove=set(json.loads((txn/'new-source-files.json').read_text()));copy=set(json.loads((txn/'old-source-files.json').read_text()))
else:raise ValueError('unknown source operation')
for n in remove:
 if not safe(n):continue
 p=dst/n
 if p.exists():
  if p.is_symlink() or not p.is_file():raise ValueError('managed destination is not a regular file')
  p.unlink()
for n in sorted(copy):
 if not safe(n):continue
 p=src/n
 if not p.is_file() or p.is_symlink():raise ValueError('source file missing: '+n)
 target=dst/n;target.parent.mkdir(parents=True,exist_ok=True)
 shutil.copy2(p,target)
manifest_file=dst/'.mp-source-files.json'
if action=='restore' and not (src/'.mp-source-files.json').exists():
 if manifest_file.exists():manifest_file.unlink()
else:manifest_file.write_text(json.dumps(sorted(copy)))
PY
}

update_started_candidate() {
  local service container marker started
  for service in api worker; do
    container=$(docker ps -aq --filter "label=com.docker.compose.project=$PROJECT" --filter "label=com.docker.compose.service=$service" | head -n 1) || return 2
    if [[ -z "$container" ]]; then
      if [[ -f "$UPDATE_TRANSACTION/candidate-attempted" && " ${OLD_RUNNING[*]} " == *" $service "* ]]; then return 2; fi
      continue
    fi
    marker=$(docker inspect --format '{{index .Config.Labels "mp.update.id"}}' "$container") || return 2
    started=$(docker inspect --format '{{.State.StartedAt}}' "$container") || return 2
    if [[ "$marker" == "$UPDATE_ID" && "$started" != 0001-* ]]; then return 0; fi
  done
  return 1
}

update_restore() {
  local restore_data=false failed=false started_status
  printf 'Восстанавливаем прежнюю версию панели. Копия: %s\n' "$UPDATE_TRANSACTION" >&2
  if [[ -f "$UPDATE_TRANSACTION/new-app-started" ]]; then
    restore_data=true
  elif update_started_candidate; then
    restore_data=true
    touch "$UPDATE_TRANSACTION/new-app-started" || return 1
    sync -f "$UPDATE_TRANSACTION/new-app-started" || return 1
  else
    started_status=$?
    if [[ "$started_status" == 2 ]]; then
      update_error 'Не удалось определить, запускалась ли новая версия. Восстановление остановлено до проверки состояния Docker.'
      return 1
    fi
  fi
  update_phase rolling-back || return 1
  if [[ -f "$UPDATE_TRANSACTION/maintenance" ]]; then
    update_compose stop -t 60 api worker || { update_error 'Не удалось остановить API/worker. База и ключи не будут восстановлены поверх работающих процессов.'; return 1; }
    update_verify_stopped || return 1
  fi
  if ! docker image inspect "$OLD_API_IMAGE" "$OLD_WORKER_IMAGE" >/dev/null 2>&1; then
    docker image load -i "$UPDATE_TRANSACTION/old-images.tar" >/dev/null || return 1
  fi
  if $restore_data; then
    [[ -f "$UPDATE_TRANSACTION/backup.ready" && -s "$UPDATE_TRANSACTION/database.dump" && -s "$UPDATE_TRANSACTION/panel-data.tar" ]] || { update_error 'Новая версия запускалась, но нет полной резервной копии. Автоматический откат остановлен.'; return 1; }
    if [[ ! -f "$UPDATE_TRANSACTION/data-restore-in-progress" ]]; then
      update_check_jobs || { update_error 'После запуска новой версии появились незавершённые задания. Восстановление базы остановлено, чтобы не потерять управляющие операции.'; return 1; }
      touch "$UPDATE_TRANSACTION/data-restore-in-progress" || return 1
      sync -f "$UPDATE_TRANSACTION/data-restore-in-progress" || return 1
    fi
    docker exec "$POSTGRES_CONTAINER" dropdb -U "$DB_USER" --if-exists --force "$DB_NAME" || return 1
    docker exec "$POSTGRES_CONTAINER" createdb -U "$DB_USER" -O "$DB_USER" "$DB_NAME" || return 1
    docker exec -i "$POSTGRES_CONTAINER" pg_restore -U "$DB_USER" -d "$DB_NAME" --no-owner --no-privileges --exit-on-error < "$UPDATE_TRANSACTION/database.dump" || return 1
    docker run --rm --network none --user 0:0 --entrypoint sh --mount "type=volume,src=$PANEL_VOLUME,dst=/data" --mount "type=bind,src=$UPDATE_TRANSACTION,dst=/backup,readonly" "$OLD_API_IMAGE" -c 'find /data -mindepth 1 -maxdepth 1 -exec rm -rf -- {} + && tar -xpf /backup/panel-data.tar -C /data' || return 1
    touch "$UPDATE_TRANSACTION/data-restored"
  fi
  if [[ -f "$UPDATE_TRANSACTION/sources-replaced" ]]; then
    update_copy_source restore "$UPDATE_TRANSACTION/previous" "$DESTINATION" || return 1
  fi
  update_image_override "$DESTINATION/.mp-runtime-images.json" "$OLD_API_IMAGE" "$OLD_WORKER_IMAGE" "rollback-$UPDATE_ID" || return 1
  if [[ -f "$UPDATE_TRANSACTION/maintenance" && ${#OLD_RUNNING[@]} -gt 0 ]]; then
    update_compose up -d --no-build --no-deps --wait --wait-timeout 240 "${OLD_RUNNING[@]}" || failed=true
  fi
  $failed && return 1
  update_phase rolled-back || return 1
  printf 'Прежняя версия и настройки восстановлены. Повторите обновление после исправления причины сбоя.\n' >&2
}

update_abort() {
  local status=$1 line=$2
  trap - ERR INT TERM
  set +e
  printf 'Обновление прервано (%s, строка %s).\n' "$status" "$line" >&2
  if [[ -f "$UPDATE_TRANSACTION/state" ]]; then
    if ! update_restore; then
      update_phase rollback-failed
      printf 'Автоматическое восстановление не завершилось. Данные копии сохранены. Выполните: sudo bash %q --recover %q\n' "$UPDATE_TRANSACTION/recover.sh" "$UPDATE_TRANSACTION" >&2
    fi
  fi
  exit "$status"
}

update_panel() {
  PROJECT=${MP_COMPOSE_PROJECT:-mp-panel}
  [[ "$PROJECT" =~ ^[a-z0-9][a-z0-9_-]*$ ]] || { update_error 'Неверное имя Compose-проекта.'; return 1; }
  command -v python3 >/dev/null || { apt-get update; apt-get install -y python3; }
  command -v flock >/dev/null || { update_error 'Нужен flock из пакета util-linux.'; return 1; }
  local root=/var/lib/mp-panel-updates
  update_guard_directory "$root" || { update_error 'Небезопасный путь резервных копий.'; return 1; }
  update_guard_directory "$root/$PROJECT" || { update_error 'Небезопасный путь каталога проекта.'; return 1; }
  mkdir -p "$root/$PROJECT";chmod 700 "$root" "$root/$PROJECT"
  [[ "$(stat -c %u "$root")" == 0 ]] || return 1
  [[ "$(stat -c %u "$root/$PROJECT")" == 0 && ! -L "$root/$PROJECT" ]] || return 1
  [[ ! -L "$root/$PROJECT/update.lock" && ( ! -e "$root/$PROJECT/update.lock" || "$(stat -c %h "$root/$PROJECT/update.lock")" == 1 ) ]] || return 1
  exec {UPDATE_LOCK_FD}>"$root/$PROJECT/update.lock"
  flock -n "$UPDATE_LOCK_FD" || { update_error 'Уже выполняется другое обновление или восстановление.'; return 1; }
  UPDATE_ID="$(date -u +%Y%m%dT%H%M%SZ)-$(openssl rand -hex 4)"
  UPDATE_TRANSACTION="$root/$PROJECT/$UPDATE_ID"
  mkdir -m 700 "$UPDATE_TRANSACTION"
  install -m 700 "$SOURCE/deploy/update.sh" "$UPDATE_TRANSACTION/recover.sh"
  export MP_UPDATE_DESTINATION="$DESTINATION"
  local api worker postgres source_size panel_size db_size free required docker_root docker_free service cid state image config_files file
  api=$(docker ps -aq --filter "label=com.docker.compose.project=$PROJECT" --filter label=com.docker.compose.service=api | head -n 1)
  worker=$(docker ps -aq --filter "label=com.docker.compose.project=$PROJECT" --filter label=com.docker.compose.service=worker | head -n 1)
  postgres=$(docker ps -q --filter "label=com.docker.compose.project=$PROJECT" --filter label=com.docker.compose.service=postgres | head -n 1)
  [[ -n "$api" && -n "$worker" && -n "$postgres" ]] || { update_error 'Нужны существующие контейнеры API/worker и работающий PostgreSQL.'; return 1; }
  [[ "$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project.working_dir"}}' "$api")" == "$DESTINATION" ]] || { update_error 'Контейнеры принадлежат другому каталогу установки.'; return 1; }
  POSTGRES_CONTAINER=$postgres
  OLD_API_IMAGE=$(docker inspect --format '{{.Image}}' "$api")
  OLD_WORKER_IMAGE=$(docker inspect --format '{{.Image}}' "$worker")
  PANEL_VOLUME=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/var/lib/mp"}}{{if eq .Type "volume"}}{{.Name}}{{end}}{{end}}{{end}}' "$api")
  [[ "$PANEL_VOLUME" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || { update_error 'panel-data должен быть именованным Docker volume.'; return 1; }
  [[ "$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/var/lib/mp"}}{{.Name}}{{end}}{{end}}' "$worker")" == "$PANEL_VOLUME" ]] || { update_error 'API и worker используют разные тома данных.'; return 1; }
  config_files=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}' "$api")
  OLD_FILES=();IFS=',' read -r -a original_files <<< "$config_files"
  for file in "${original_files[@]}"; do
    [[ "$file" != "$DESTINATION/.mp-runtime-images.json" ]] || continue
    [[ "$file" == /* && -f "$file" && ! -L "$file" && "$(stat -c %h "$file")" == 1 ]] || { update_error 'Не удалось безопасно прочитать действующий Compose-файл.'; return 1; }
    OLD_FILES+=("$file")
  done
  [[ ${#OLD_FILES[@]} -gt 0 ]] || return 1
  update_compose config --format json > "$UPDATE_TRANSACTION/old-compose.json"
  read -r DB_NAME DB_USER < <(python3 - "$UPDATE_TRANSACTION/old-compose.json" <<'PY'
import json,sys
env=json.load(open(sys.argv[1]))['services']['postgres']['environment']
print(env.get('POSTGRES_DB','mp'),env.get('POSTGRES_USER','mp'))
PY
  )
  [[ "$DB_NAME" =~ ^[A-Za-z0-9_][A-Za-z0-9_-]*$ && "$DB_NAME" != postgres && "$DB_NAME" != template* && "$DB_USER" =~ ^[A-Za-z0-9_][A-Za-z0-9_-]*$ ]] || return 1
  OLD_RUNNING=()
  for service in api worker; do
    [[ "$service" != api ]] || cid=$api
    [[ "$service" != worker ]] || cid=$worker
    state=$(docker inspect --format '{{.State.Status}}' "$cid")
    [[ "$state" != paused && "$state" != restarting ]] || { update_error 'Сначала приведите контейнеры API/worker в стабильное состояние.'; return 1; }
    [[ "$state" != running ]] || OLD_RUNNING+=("$service")
  done
  update_check_jobs
  [[ -z "$(find "$DESTINATION" -type l -print -quit)" && -z "$(find "$DESTINATION" -type f -links +1 -print -quit)" ]] || { update_error 'Каталог установки содержит ссылки. Разберите их перед обновлением.'; return 1; }
  source_size=$(du -sb "$DESTINATION" | awk '{print $1}')
  panel_size=$(docker run --rm --network none --user 0:0 --entrypoint sh --mount "type=volume,src=$PANEL_VOLUME,dst=/data,readonly" "$OLD_API_IMAGE" -c 'du -sb /data' | awk '{print $1}')
  db_size=$(docker exec "$POSTGRES_CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -Atqc 'SELECT pg_database_size(current_database())')
  [[ "$source_size" =~ ^[0-9]+$ && "$panel_size" =~ ^[0-9]+$ && "$db_size" =~ ^[0-9]+$ ]] || return 1
  free=$(df -Pk "$root" | awk 'NR==2{printf "%.0f",$4*1024}')
  required=$((source_size*3+panel_size+db_size*2+4294967296))
  (( free >= required )) || { update_error 'Недостаточно свободного места для копий и сборки (нужно минимум 4 GiB плюс размер данных).'; return 1; }
  docker_root=$(docker info --format '{{.DockerRootDir}}')
  docker_free=$(df -Pk "$docker_root" | awk 'NR==2{printf "%.0f",$4*1024}')
  (( docker_free >= 4294967296 )) || { update_error 'В файловой системе Docker нужно минимум 4 GiB свободного места для сборки.'; return 1; }
  update_phase preparing
  mkdir -m 700 "$UPDATE_TRANSACTION/previous" "$UPDATE_TRANSACTION/candidate"
  cp -a -- "$DESTINATION/." "$UPDATE_TRANSACTION/previous/"
  cp -a -- "$DESTINATION/." "$UPDATE_TRANSACTION/candidate/"
  update_copy_source candidate "$SOURCE" "$UPDATE_TRANSACTION/candidate"
  docker image tag "$OLD_API_IMAGE" "mp-panel-backup:$UPDATE_ID-api"
  docker image tag "$OLD_WORKER_IMAGE" "mp-panel-backup:$UPDATE_ID-worker"
  docker image save -o "$UPDATE_TRANSACTION/old-images.tar" "$OLD_API_IMAGE" "$OLD_WORKER_IMAGE"
  update_state
  trap 'update_abort $? "$LINENO"' ERR
  trap 'update_abort 130 "$LINENO"' INT
  trap 'update_abort 143 "$LINENO"' TERM
  python3 - "$UPDATE_TRANSACTION/old-compose.json" "$UPDATE_TRANSACTION/build.compose.json" "$DESTINATION" "$UPDATE_TRANSACTION/candidate" "$UPDATE_ID" <<'PY'
import json,sys
from pathlib import Path
source,target,installation,candidate,identifier=sys.argv[1:]
data=json.load(open(source))
for service in ['api','worker']:
 spec=data['services'][service];build=spec.get('build')
 if not isinstance(build,dict):raise ValueError('API and worker need local build configuration')
 old_context=Path(build.get('context',''))
 relative=old_context.relative_to(Path(installation))
 build['context']=str(Path(candidate)/relative)
 spec['image']='mp-panel-update:'+identifier+'-'+service
json.dump(data,open(target,'w'))
PY
  update_phase building
  printf 'Собираем новую версию, прежняя панель продолжает работать.\n'
  docker compose --project-name "$PROJECT" --project-directory "$UPDATE_TRANSACTION/candidate" -f "$UPDATE_TRANSACTION/build.compose.json" build api worker
  update_check_jobs
  update_phase maintenance
  touch "$UPDATE_TRANSACTION/maintenance"
  sync -f "$UPDATE_TRANSACTION/maintenance"
  update_compose stop -t 60 api worker
  update_verify_stopped
  update_check_jobs
  printf 'Создаём согласованную копию PostgreSQL и ключей панели.\n'
  docker exec "$POSTGRES_CONTAINER" pg_dump -U "$DB_USER" -d "$DB_NAME" --format custom --no-owner --no-privileges > "$UPDATE_TRANSACTION/database.dump.new"
  docker exec -i "$POSTGRES_CONTAINER" pg_restore --list < "$UPDATE_TRANSACTION/database.dump.new" > "$UPDATE_TRANSACTION/database.contents"
  [[ -s "$UPDATE_TRANSACTION/database.dump.new" ]]
  mv -- "$UPDATE_TRANSACTION/database.dump.new" "$UPDATE_TRANSACTION/database.dump"
  docker run --rm --network none --user 0:0 --entrypoint sh --mount "type=volume,src=$PANEL_VOLUME,dst=/data,readonly" --mount "type=bind,src=$UPDATE_TRANSACTION,dst=/backup" "$OLD_API_IMAGE" -c 'tar -cpf /backup/panel-data.tar.new -C /data .'
  tar -tf "$UPDATE_TRANSACTION/panel-data.tar.new" > "$UPDATE_TRANSACTION/panel-data.contents"
  mv -- "$UPDATE_TRANSACTION/panel-data.tar.new" "$UPDATE_TRANSACTION/panel-data.tar"
  sync -f "$UPDATE_TRANSACTION/database.dump";sync -f "$UPDATE_TRANSACTION/panel-data.tar"
  touch "$UPDATE_TRANSACTION/backup.ready"
  sync -f "$UPDATE_TRANSACTION/backup.ready"
  update_phase applying
  touch "$UPDATE_TRANSACTION/sources-replaced"
  sync -f "$UPDATE_TRANSACTION/sources-replaced"
  update_copy_source apply "$UPDATE_TRANSACTION/candidate" "$DESTINATION"
  update_image_override "$DESTINATION/.mp-runtime-images.json" "mp-panel-update:$UPDATE_ID-api" "mp-panel-update:$UPDATE_ID-worker" "$UPDATE_ID"
  if (( ${#OLD_RUNNING[@]} )); then
    touch "$UPDATE_TRANSACTION/candidate-attempted"
    sync -f "$UPDATE_TRANSACTION/candidate-attempted"
    update_compose up -d --no-build --no-deps --wait --wait-timeout 240 "${OLD_RUNNING[@]}"
    touch "$UPDATE_TRANSACTION/new-app-started"
    sync -f "$UPDATE_TRANSACTION/new-app-started"
    # worker has no Compose healthcheck. Check it remains running rather than
    # declaring success during its first instant before an immediate crash.
    local initial_worker_restarts='' worker_container=''
    if [[ " ${OLD_RUNNING[*]} " == *' worker '* ]]; then
      worker_container=$(docker ps -q --filter "label=com.docker.compose.project=$PROJECT" --filter label=com.docker.compose.service=worker | head -n 1)
      [[ -n "$worker_container" ]]
      initial_worker_restarts=$(docker inspect --format '{{.RestartCount}}' "$worker_container")
      sleep 5
      [[ "$(docker inspect --format '{{.State.Running}}:{{.RestartCount}}' "$worker_container")" == "true:$initial_worker_restarts" ]]
    fi
  fi
  update_phase committed
  trap - ERR INT TERM
  printf 'Обновление завершено. Резервная копия сохранена в %s.\n' "$UPDATE_TRANSACTION"
  printf 'Настройки .env и действующие Compose-файлы сохранены. Остановленные ранее сервисы остаются остановленными.\n'
}

update_recover() {
  [[ $EUID -eq 0 ]] || { update_error 'Восстановление требует root.'; return 1; }
  local target=$1 root=/var/lib/mp-panel-updates project identifier
  [[ "$target" =~ ^/var/lib/mp-panel-updates/([a-z0-9][a-z0-9_-]*)/([0-9]{8}T[0-9]{6}Z-[a-f0-9]{8})$ ]] || { update_error 'Неверный путь транзакции.'; return 1; }
  project=${BASH_REMATCH[1]};identifier=${BASH_REMATCH[2]}
  update_guard_directory "$target" || return 1
  [[ -f "$target/state" && ! -L "$target/state" && "$(stat -c %u "$target/state")" == 0 && "$(stat -c %h "$target/state")" == 1 && "$(stat -c %a "$target/state")" == 600 ]] || return 1
  [[ "$(stat -c %u "$target")" == 0 && "$(stat -c %a "$target")" == 700 ]] || return 1
  [[ ! -L "$root/$project/update.lock" && "$(stat -c %h "$root/$project/update.lock")" == 1 ]] || return 1
  exec {UPDATE_LOCK_FD}>"$root/$project/update.lock";flock -n "$UPDATE_LOCK_FD" || { update_error 'Обновление или восстановление уже выполняется.'; return 1; }
  # The state was generated by declare -p, inside a root-owned 0700 directory.
  source "$target/state"
  [[ "$UPDATE_TRANSACTION" == "$target" && "$PROJECT" == "$project" ]] || return 1
  UPDATE_ID=$identifier;export MP_UPDATE_DESTINATION="$DESTINATION"
  update_guard_directory "$DESTINATION" || return 1
  local phase
  phase=$(cat "$target/phase")
  if [[ "$phase" == committed || "$phase" == rolled-back ]]; then
    update_error 'Транзакция уже завершена. Восстановление её данных поверх работающей панели не выполняется.';return 1
  fi
  update_restore
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  set -Eeuo pipefail
  umask 077
  [[ ${1:-} == --recover && $# == 2 ]] || { printf 'Использование: sudo bash recover.sh --recover /var/lib/mp-panel-updates/PROJECT/ID\n' >&2;exit 1; }
  update_recover "$2"
fi
