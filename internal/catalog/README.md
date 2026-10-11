# Official distribution catalog

The catalog makes upstream requests on demand. Metadata is cached for 15 minutes, bounded to 256 responses. A refresh expires entries while retaining them as a fallback during upstream outages. Without a cached response, an unavailable provider returns an explicit error; no versions or URLs are fabricated.

Requests identify MP through the configured `MP_DOWNLOAD_USER_AGENT`, have a 20-second timeout, an 8 MiB metadata cap, HTTPS-only redirects to known official domains, and a six-request concurrency limit. Fabric launcher and BuildTools binaries are streamed once during resolution to compute SHA-256 (32 MiB limit), then downloaded and checked by the agent during installation.

Resolved plans pin Minecraft version, loader/build, installer, Java version, artifact URL and checksum. Installed servers are never upgraded by catalog refresh. Stable releases appear before previews. The resolver chooses a recommended build when no build was supplied, and requires explicit selection if no recommendation exists.

## Sources and profiles

- **Vanilla:** [Mojang version manifest](https://piston-meta.mojang.com/mc/game/version_manifest_v2.json). Per-version metadata provides server URL, size, SHA-1 and Java requirement. Releases from 1.12.2 onward are listed.
- **Paper:** [Fill v3](https://docs.papermc.io/misc/downloads-service/). Official build channels distinguish stable and experimental builds. The first stable build is recommended. [Paper Java recommendations](https://docs.papermc.io/paper/getting-started/) supplement Mojang requirements for old releases.
- **Fabric:** [Fabric Meta](https://github.com/FabricMC/fabric-meta/blob/master/README.md), compatible loader list and current stable installer. The [official executable server launcher](https://fabricmc.net/use/?page=server) pins all three versions. Loaders older than 0.12.5 are excluded from this installation profile. Fabric API mods must be supplied separately when required by the user's mods.
- **Forge:** [Official Maven metadata](https://maven.minecraftforge.net/net/minecraftforge/forge/maven-metadata.xml) and [promotions](https://files.minecraftforge.net/net/minecraftforge/forge/promotions_slim.json). Exact Maven versions retain any legacy branch suffix. Installer JAR SHA-1 is read from official Maven checksums.
- **NeoForge:** [Official Maven metadata](https://maven.neoforged.net/releases/net/neoforged/neoforge/maven-metadata.xml). The old three-component numbering maps `20.2.N` to Minecraft `1.20.2`; the [26.1 numbering](https://neoforged.net/news/26.1release/) maps `26.1.0.N` to `26.1` and `26.1.1.N` to `26.1.1`. Unknown numbering schemes are rejected. Legacy NeoForge 1.20.1 uses a different Maven group and is outside this profile.
- **Spigot:** [Official revision metadata](https://hub.spigotmc.org/versions/) and [BuildTools](https://www.spigotmc.org/wiki/buildtools/). The exact Jenkins BuildTools artifact is pinned. `Build` records the source revision reported by upstream; BuildTools is invoked with `--rev <Minecraft version>`. Upstream controls source metadata during compilation, so full source reproducibility is not guaranteed by this profile.
- **Custom:** The API accepts the user's JAR through the authenticated upload flow; Java 8, 11, 16, 17, 21 or 25 is mandatory. This profile does not infer loader compatibility or download third-party artifacts.

Java 25 is discovered for Minecraft 26.1+ through current Mojang metadata. Core-specific installation mode is `direct`, `forge`, `neoforge`, or `spigot`. Fabric launches `fabric-server-launch.jar`; other direct profiles launch `server.jar`.
