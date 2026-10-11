# MP API contract

Base `/api/v1`. JSON snake_case. Lists are JSON arrays. Errors `{error:string,code?:string}`. Async mutations return `202` with a Job object.

## Authentication
- `GET /auth/status` -> `{initialized,authenticated,user?,csrf_token?}`
- `POST /auth/setup` `{username,password,setup_token}` -> same authenticated status (only first owner; token from data/keys/setup.token).
- `POST /auth/login` `{username,password,code?}` -> status; TOTP challenge error `code:totp_required`.
- `POST /auth/logout`; `GET /auth/me` -> User.
- `POST /auth/totp/setup` -> `{secret,uri}`; `POST /auth/totp/enable` `{code}` -> `{recovery_codes:string[]}`.
- `POST /auth/totp/disable` `{password,code}`.
Cookie session, mutations require `X-CSRF-Token` returned by status/login/setup. API client credentials same-origin.

## Nodes
- `GET /nodes` -> Node[]
- `POST /nodes/bootstrap` `{name,address,ssh_port,ssh_user,password?,private_key?,host_fingerprint,sudo_password?,game_bind_address?}` -> Job. Game binding is a local IP on that node, default `0.0.0.0`; an omitted reconnect field preserves the saved address.
- `POST /nodes/{id}/bootstrap` same payload -> Job; reconnect existing identity, fresh SSH credentials required.
- `POST /nodes/fingerprint` `{address,ssh_port,ssh_user}` -> `{fingerprint}` (display it before bootstrap).
- `GET /nodes/{id}` -> Node; `DELETE /nodes/{id}` only when it has no managed servers.

Node `{id,name,address,status,ssh_port,ssh_user,game_bind_address?,os?,arch?,cpu_count?,memory_total?,disk_total?,disk_free?,last_seen?,error?,created_at}`; status connecting/online/offline/error. Byte metric units. Operators see only hosts associated with assigned servers, with host-wide resources and SSH details redacted; host metrics are denied.

## Live catalog
- `GET /catalog/cores` -> `[{id,name,description,kind}]`
- `GET /catalog/cores/{core}/versions` -> `[{id,stable,supported,note?}]`
- `GET /catalog/cores/{core}/versions/{version}/builds` -> `[{id,name,stable,recommended,java_version,installer_version?}]`
- `POST /catalog/resolve` `{core,minecraft_version,loader_version?,build?,java_version?}` -> InstallSpec.
- `POST /catalog/refresh` -> `{ok:true}` (owner).
- Metadata is cached for 15 minutes with last successful response fallback during provider outages; no fabricated versions.

## Servers
- `GET /servers` -> Server[]
- `POST /servers` `{name,node_id,core,minecraft_version,loader_version?,build?,java_version?,memory_mb,cpu_limit,port,eula_accepted,jar_upload_id?,extra_upload_ids?:string[]}` -> Job (contains server_id).
- `GET /servers/{id}` -> Server
- `GET/PUT /servers/{id}/operators` -> `{user_ids:string[]}`; owner-only. PUT replaces the entire assignment list; IDs must belong to existing operator users. `[]` revokes all operator assignments.
- `POST /servers/{id}/actions` `{action:"start"|"stop"|"restart"|"delete"}` -> Job.
- `POST /servers/{id}/installations` same version fields, `eula_accepted:true` -> Job; backup before install.
- `GET /servers/{id}/metrics` -> Metric[]; `GET /nodes/{id}/metrics` -> Metric[].
Server `{id,node_id,name,core,minecraft_version,loader_version,build,java_version,memory_mb,cpu_limit,port,status,desired_state,eula_accepted,created_at,error?}`.
Metric `{timestamp,node_id,server_id?,cpu_percent,memory_used,memory_limit,disk_used,disk_free,io_read,io_write,uptime_seconds?}`.

## Jobs
- `GET /jobs` -> Job[]; `GET /jobs/{id}` -> Job.
Job `{id,node_id,server_id?,type,status,step,error?,logs:string[],created_at,updated_at}`; status queued/running/succeeded/failed.

## Files, properties and extensions
- `POST /uploads` multipart `file` -> `{id,name,size,sha256}` (streamed to disk; no application upload size limit).
- `GET /servers/{id}/files?path=...` -> `[{name,path,is_dir,size,modified}]`
- `GET /servers/{id}/files/content?path=...` -> `{path,content,revision}` (text limit).
- `PUT /servers/{id}/files/content` `{path,content,revision?}` -> `{ok:true}`.
- `POST /servers/{id}/files/upload` `{path,upload_id}` -> Job (stopped server).
- `POST /servers/{id}/files/upload-batch` `{files:[{path,upload_id}]}` -> Job; upload each file to `/uploads` first. Nested paths create missing directories.
- `POST /servers/{id}/files/transfer` `{paths:string[],destination:string,mode:"copy"|"move"}` -> Job. Destination must exist; copy to the source directory creates a unique copy name.
- `POST /servers/{id}/files/extract` `{path,destination}` -> Job; ZIP or single-volume unencrypted RAR. Destination directories are created when missing.
- `POST /servers/{id}/files/delete-batch` `{paths:string[]}` -> Job; directories are removed with their contents.
- `POST /servers/{id}/files/directory` `{path}` -> `{ok:true}`.
- `POST /servers/{id}/files/rename` `{path,new_path}` -> `{ok:true}`.
- `DELETE /servers/{id}/files?path=...` -> `{ok:true}` (stopped server).
- `GET /servers/{id}/properties` -> `{properties:Record<string,string>,revision}`.
- `PUT /servers/{id}/properties` `{properties,revision}` -> `{ok:true}` (restart needed).
- `GET /servers/{id}/extensions` -> FileEntry[] from mods/plugins.
- `POST /servers/{id}/extensions` `{upload_id}` -> Job.
- `POST /servers/{id}/extensions/toggle` `{path,enabled}` -> `{ok:true}`.

Batch operations require management permission, an online node and a stopped server (verified again by the agent). They return `202` and a Job. Upload commands have a 24-hour deadline; other batch operations have a 20-minute deadline. Uploads have no application limit on file count or size. Other batch operations accept at most 100 selected objects. Upload request bodies and authenticated command manifests are streamed without a fixed body-size ceiling; metadata still requires memory proportional to file count. Paths are canonical UTF-8 relative paths; empty destination means the server root. Duplicate or overlapping selections, symlinks, special files, traversal and existing destination files are rejected. Copy/move source trees are capped at 100,000 entries. Archives are capped at 10,000 entries including implicit parent directories and 2GiB expanded data; RAR dictionaries are limited to 64MiB. ZIP metadata is capped at 64MiB. Files are staged before commit; ordinary operation errors trigger rollback. Process/host termination during commit may require administrative recovery. Folder uploads preserve file paths but do not transfer empty directories. Agents must be upgraded along with the panel to support these commands.

## Console and backups
- `GET /servers/{id}/logs` -> `{lines:string[]}`
- `POST /servers/{id}/console` `{command}` -> `{ok:true}`
- `WS /servers/{id}/console` outputs `{lines:string[]}`, accepts `{command}`; cookie + Origin checked.
- `GET /servers/{id}/backups` -> Backup[].
- `POST /servers/{id}/backups` `{name?}` -> Job (offline backup).
- `POST /servers/{id}/restores` `{backup_id}` -> Job (stopped).
Backup `{id,server_id,node_id,name,size,sha256,created_at,path}`.
- `GET /audit` -> AuditEvent[].
- `GET /users`; `POST /users` `{username,password,role}`; roles owner/admin/operator/viewer.
- `GET /settings` -> `{agent_public_url,default_java,version,gateway_enabled,gateway_provider,game_domain}`.
- `GET /updates` -> `{current_version,latest_version?,update_available,release_url?,published_at?,checked_at?,error?}` (owner-only). Returns the cached state immediately and refreshes stale data in the background.
- `POST /updates/check` -> same status (owner-only, CSRF required). Stable releases are checked on GitHub, at most once per minute; successful results are cached for six hours. Network failures retain previously validated release details and do not block the panel. These endpoints do not install updates or execute host commands.
- AuditEvent `{id,actor,action,object_id,detail,created_at}`. Owner/admin manage all servers, viewer reads globally. Operator sees and fully manages only explicitly assigned servers and their jobs; server creation, host administration, users and access assignment remain restricted. Uploads are owned by the uploading user. Owner cannot be created through `/users`.

## Optional Minecraft gateway
- `GET /servers/{id}/route` -> `{enabled:boolean,provider:string,base_domain:string,default_domain?:string,route?:Route}`.
- `PUT /servers/{id}/route` `{domain,backend?}` -> 202 Route; backend defaults to concrete node game_bind_address (otherwise SSH address) and published game port.
- `DELETE /servers/{id}/route` -> `{ok:true}`; removal is reconciled asynchronously.
- Route `{server_id,name?,external?:boolean,domain,backend,status,error?,updated_at}`; status pending/active/error/removing.
- `GET /external-routes` -> `{enabled,provider,base_domain,routes:Route[]}` (owner/admin only; external routes only).
- `POST /external-routes` `{name,domain,backend}` -> 202 Route; no server/node/agent is created. The route ID is returned in `server_id` with an `external-` prefix.
- `PUT /external-routes/{id}` `{name,domain,backend}` -> 202 Route; managed routes cannot be changed through this endpoint.
- `DELETE /external-routes/{id}` -> 202 `{ok:true}`; removes a connection route, not the backend server.
- mc-router or Gate applies explicit Minecraft Java hostname routes. DNS, NAT, VPN and firewall are configured separately. Active means applied rule, not verified end-to-end game access. Route count is capped at 2000; mc-router retains domain ownership history (4000 domains) to reconcile uncertain deletions after restart.

## Agent interface (mTLS only, separate listener)
- `POST /agent/v1/heartbeat` Heartbeat.
- `GET /agent/v1/commands?wait=20` -> Command or 204.
- `POST /agent/v1/results` CommandResult.
- `GET /agent/v1/artifacts/{id}` streams upload (command node must match).
Model types in `internal/model/model.go` are the canonical runtime contract.
