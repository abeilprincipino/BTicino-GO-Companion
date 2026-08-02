# Build e installazione da locale

Procedura per compilare il companion sulla workstation e installarlo su un dispositivo
BTicino, senza passare da una release GitHub.

Tutti i comandi in questo documento sono stati eseguiti e verificati su Windows
(Git Bash, Go 1.26.4). Le trappole specifiche di Windows sono in §5 — **leggile prima di
copiare `gst/`**, perché una copia diretta produce un runtime GStreamer rotto in modo
silenzioso.

---

## 0. Riferimento rapido (one-liner)

Sostituire `<IP>` con host o indirizzo del dispositivo. I comandi Windows sono per **Git Bash**
salvo dove indicato.

**Build** — Git Bash:

```sh
cd /d/Progetti/BTicinoGO && CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -buildvcs=false -trimpath -ldflags "-s -w -X bticino-go-companion/internal/system.BuildVersion=v0.0.0-local -X bticino-go-companion/internal/system.BuildGitSHA=$(git rev-parse --short=7 HEAD) -X bticino-go-companion/internal/system.BuildReleaseRepo=" -o dist/companion ./cmd/companion
```

**Build** — PowerShell:

```powershell
cd d:\Progetti\BTicinoGO; $env:CGO_ENABLED=0; $env:GOOS='linux'; $env:GOARCH='arm'; $env:GOARM='7'; go build -buildvcs=false -trimpath -ldflags "-s -w -X bticino-go-companion/internal/system.BuildVersion=v0.0.0-local -X bticino-go-companion/internal/system.BuildGitSHA=$(git rev-parse --short=7 HEAD) -X bticino-go-companion/internal/system.BuildReleaseRepo=" -o dist/companion ./cmd/companion
```

**Staging + install completo** (Git Bash o PowerShell):

```sh
git -c core.autocrlf=false archive HEAD gst scripts | ssh root@<IP> 'mkdir -p /tmp/stage && tar -x -C /tmp/stage' && scp dist/companion root@<IP>:/tmp/stage/companion && ssh root@<IP> 'sh /tmp/stage/scripts/install.sh /tmp/stage/companion'
```

**Verifica post-install:**

```sh
ssh root@<IP> '/etc/init.d/companion status; tail -30 /tmp/companion.log'
```

**Re-deploy rapido** (solo binario, gst già installato):

```sh
scp dist/companion root@<IP>:/home/bticino/cfg/extra/companion/companion.new && ssh root@<IP> 'chmod 755 /home/bticino/cfg/extra/companion/companion.new && /etc/init.d/companion restart && /etc/init.d/companion status'
```

**Rollback:**

```sh
ssh root@<IP> 'cd /home/bticino/cfg/extra/companion && cp -f companion.previous companion.new && /etc/init.d/companion restart && /etc/init.d/companion status'
```

**Riparare uno stage già copiato con CRLF** (da eseguire sul device, `<stage>` = dir che contiene `scripts/`):

```sh
cd <stage> && for f in scripts/install.sh scripts/init.d/companion; do tr -d '\r' < "$f" > "$f.lf" && mv "$f.lf" "$f"; done && sh scripts/install.sh "$PWD/companion"
```

**Controllo integrità di `gst/`** — il primo deve essere un symlink, il secondo avere la `x`:

```sh
ls -l <stage>/gst/lib/libgstaudio-1.0.so.0 <stage>/gst/bin/gst-launch-1.0
```

---

## 1. Prerequisiti

| Cosa | Versione / nota |
|---|---|
| Go | `1.26` (da [go.mod](../go.mod)); verificato con go1.26.4 |
| Accesso al dispositivo | SSH come `root` (il servizio `dropbear` è abilitato di default) |
| Spazio su `/tmp` del device | ~20 MB (binario 15,3 MB + bundle gst 3,5 MB) |
| Docker | opzionale, solo per eseguire i test (vedi §4) |

Nessun toolchain C necessario: la build è `CGO_ENABLED=0`, Go cross-compila da solo.

---

## 2. Build del binario ARMv7

Target reale del dispositivo: **linux/arm, ARMv7, statically linked**.

```sh
cd d:/Progetti/BTicinoGO

CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
go build -buildvcs=false -trimpath \
  -ldflags "-s -w \
    -X bticino-go-companion/internal/system.BuildVersion=v0.0.0-local \
    -X bticino-go-companion/internal/system.BuildGitSHA=$(git rev-parse --short=7 HEAD) \
    -X bticino-go-companion/internal/system.BuildReleaseRepo=" \
  -o dist/companion ./cmd/companion
```

Risultato verificato: `ELF 32-bit LSB executable, ARM, EABI5, statically linked, stripped`,
15,3 MB, ~34 s a freddo.

### Le tre variabili `-ldflags`

Sono lette da [internal/system](../internal/system/release.go) via `CurrentBuildInfo()`:

| Variabile | Effetto |
|---|---|
| `BuildVersion` | versione mostrata in WebUI / `GET /api/v3/state` |
| `BuildGitSHA` | commit corto, per diagnostica |
| `BuildReleaseRepo` | **se vuota, l'auto-updater è disattivato** |

Lasciare `BuildReleaseRepo` vuota è la scelta giusta per un'installazione locale:
in [app/run.go:318-321](../internal/app/run.go#L318-L321) il `ReleaseSource` resta `nil`,
`Updater.Check` restituisce `ErrUpdateUnavailable` e il goroutine `checkUpdates` termina.
Senza questo, il companion controllerebbe le release di GitHub e **sostituirebbe il tuo
binario locale** al primo update disponibile.

Per abilitare gli update: `-X ...BuildReleaseRepo=abeilprincipino/BTicino-GO-Companion`.

---

## 3. Installazione sul dispositivo

`install.sh` ha due modalità, decise dalla presenza di un argomento
([install.sh:397](../scripts/install.sh#L397)):

- **con argomento** → usa il binario locale indicato;
- **senza argomento** → scarica il bundle dall'ultima release di
  `r0bb10/BTicino-GO-Companion` (`DEFAULT_RELEASE_REPO`) e verifica lo SHA256.

Per l'install locale serve quindi **sempre** passare il path del binario.

### 3.1 Layout richiesto accanto a `install.sh`

Lo script si aspetta due percorsi relativi a sé stesso
([install.sh:9-10](../scripts/install.sh#L9-L10) e
[:375](../scripts/install.sh#L375)):

```
<stage>/scripts/install.sh          <- lo script
<stage>/scripts/init.d/companion    <- ${SCRIPT_DIR}/init.d/companion   (obbligatorio)
<stage>/gst/                        <- ${SCRIPT_DIR}/../gst             (obbligatorio al 1° install)
<stage>/companion                   <- il binario, path passato come $1
```

Se `gst/` manca, `install_gst_runtime` non fallisce — logga *"keeping existing runtime"* e
tira avanti — ma poi `post_install_checks` cerca `-x .../gst/bin/gst-launch-1.0` e
l'installazione esce con codice 1. Su una prima installazione `gst/` non è opzionale.

### 3.2 Trasferimento — usare `git archive`, non `scp -r`

```sh
# 1. scripts/ + gst/ con modi, symlink E fine riga corretti
git -c core.autocrlf=false archive HEAD gst scripts \
  | ssh root@<IP> 'mkdir -p /tmp/stage && tar -x -C /tmp/stage'

# 2. il binario appena compilato
scp dist/companion root@<IP>:/tmp/stage/companion
```

`git archive` serve perché nel working tree Windows i symlink di `gst/lib` sono file di
testo e i bit di esecuzione sono persi (§5.1); legge dall'index e riemette entrambi
correttamente.

`-c core.autocrlf=false` serve perché **`git archive` da solo non basta**: questo repo ha
`core.autocrlf=true` e nessun `.gitattributes`, quindi la conversione CRLF viene applicata
anche in export. Senza l'override si ottiene `set -eu\r` e lo script muore con
`set: -: invalid option` (§5.2).

Output verificato con l'override — LF sui due script, 18 symlink reali, bit di esecuzione intatti:

```
#!/usr/bin/env sh$          <- cat -A: nessun ^M
-rwxrwxr-x  gst/bin/gst-launch-1.0
lrwxrwxrwx  gst/lib/libgstaudio-1.0.so.0 -> libgstaudio-1.0.so.0.1405.0
```

> `git archive HEAD` esporta **il commit**, non le modifiche non committate. Se hai toccato
> `gst/` o `scripts/` senza committare, committa (o usa WSL, §5.2) prima di questo passo.

### 3.3 Esecuzione

```sh
ssh root@<IP> 'sh /tmp/stage/scripts/install.sh /tmp/stage/companion'
```

Cosa fa, in ordine ([install.sh:395-402](../scripts/install.sh#L395-L402)):

1. verifica di essere `root`, poi `mount -o remount,rw /` (ripristinato a `ro` in `trap EXIT`);
2. copia il binario in `/home/bticino/cfg/extra/companion/companion`, salvando il precedente
   come `companion.previous`;
3. installa `gst/` in `.../companion/gst`, salvando il precedente come `gst.previous`;
4. copia l'init script in `/etc/init.d/companion` e crea i symlink di boot:
   `S99zzcompanion` in `rc{2,3,4,5}.d`, `K55companion` in `rc{0,1,6}.d`;
5. apre il firewall — TCP `8080 80 8554 51826`, UDP `5353 8555` — e prova a rendere le regole
   persistenti patchando `/etc/network/if-pre-up.d/iptables` (se il marker non c'è, `warn` e
   prosegue: le regole resteranno solo fino al reboot);
6. `restart` del servizio; poi, se `COMPANION_SIP_INBOUND` non è `0`, provisioning dell'utente
   SIP `companion` in flexisip (§8): attesa dell'health (fino a
   `COMPANION_HEALTHCHECK_TIMEOUT_SEC`) il tempo che il companion scriva `config.yaml`, e un
   secondo `restart` **solo se** `config.yaml` è stato effettivamente modificato. Con
   `COMPANION_SIP_INBOUND=0` niente di tutto questo viene eseguito;
7. post-install check, incluso l'health su `http://127.0.0.1:8080/api/v3/health` con timeout
   45 s (`COMPANION_HEALTHCHECK_TIMEOUT_SEC`).

Exit code `1` se almeno un check fallisce; il dettaglio è nelle righe `FAIL:`.

### 3.4 Verifica

```sh
ssh root@<IP> '/etc/init.d/companion status; tail -30 /tmp/companion.log'
```

- **WebUI** — `http://<IP>/` — al primo accesso chiede di creare l'account admin
  (`POST /webui/api/bootstrap/account`).
- **API** — `http://<IP>:8080/api/v3/health` — senza auth.
- **Configurazione** — `/home/bticino/cfg/extra/companion/config.yaml`, creato al primo avvio.
  Non serve prepararlo: `config.Default` semina un entrypoint (`id: main`, `devaddr: 20`,
  stream/unlock/ring attivi) e genera `instance_id`; il PIN HomeKit viene generato al primo
  avvio del bridge.

---

## 4. Re-deploy rapido (solo binario)

Quando `gst/` è già installato, l'init script sa promuovere un binario staged: `do_start`
chiama `activate_staged_binary`, che sposta `companion.new` su `companion` tenendo un
hardlink come `companion.previous` ([init.d/companion:134-142](../scripts/init.d/companion#L134-L142)).

```sh
scp dist/companion root@<IP>:/home/bticino/cfg/extra/companion/companion.new
ssh root@<IP> 'chmod 755 /home/bticino/cfg/extra/companion/companion.new \
  && /etc/init.d/companion restart && /etc/init.d/companion status'
```

Questo è il loop di iterazione veloce: salta firewall, symlink di boot e reinstallazione di gst.

### Rollback

```sh
ssh root@<IP> 'cd /home/bticino/cfg/extra/companion \
  && cp -f companion.previous companion.new \
  && /etc/init.d/companion restart'
```

`gst.previous` è il corrispettivo per il runtime GStreamer.

### Watchdog

L'init script avvia un watchdog che ogni 15 s verifica processo + health e, se il companion
è morto o non risponde, lo riavvia (backoff esponenziale fino a 120 s). Va tenuto presente
durante il debug: **non sostituire il binario a mano senza passare da `restart`**, o il
watchdog riavvierà la versione vecchia. `stop` disattiva anche il watchdog.

Il watchdog parte anche quando lo start iniziale fallisce, così un boot in cui la partizione
`cfg/extra` non è ancora montata non lascia il device senza supervisione: sarà il watchdog a
riprovare finché il binario compare.

### Sequenza di boot

`start` prima di lanciare il companion:

1. attende fino a 60 s che `companion` (o `companion.new`) sia eseguibile su
   `/home/bticino/cfg/extra` — quella partizione può essere montata dopo l'init script.
   Override con `COMPANION_BINARY_WAIT_SECONDS`;
2. attende fino a 30 s l'IP LAN (`wlan0`, poi `eth0`) e avvia `flexisipsh` se il proxy SIP
   locale non è già in ascolto su `127.0.0.1:5060`. Serve che flexisip sia su **prima** che
   lo stack BTicino registri, così il c100x registra localmente e il path SIP della camera
   funziona senza cloud; l'attesa dell'IP ritarda anche `bt_daemon` quanto basta a vincere
   la race. Il watchdog ripete il controllo flexisip a ogni ciclo.

---

## 5. Trappole su Windows

### 5.1 `gst/` non si può copiare dal working tree

Questo repo ha `core.symlinks=false` e `core.fileMode=false`. Conseguenze verificate sul
checkout attuale:

| Problema | Entità |
|---|---|
| symlink → file di testo | 18 file in `gst/lib` (es. `libgstaudio-1.0.so.0` è un file da 27 byte con dentro `libgstaudio-1.0.so.0.1405.0`) |
| bit di esecuzione perso | 17 file, incluso `gst/bin/gst-launch-1.0` e `gst/libexec/gstreamer-1.0/gst-plugin-scanner` |

Un `scp -r gst/` da qui produce un runtime che **non fallisce all'installazione** ma si rompe
a runtime: il loader non risolve i SONAME e `gst-launch-1.0` non è eseguibile, quindi
l'`AudioBridge` non parte → niente audio bidirezionale né snapshot. Il `post_install_checks`
intercetta almeno il caso `-x` mancante.

Soluzione: `git archive` (§3.2). Alternative: `tar` da WSL, oppure riparare sul device con
`chmod -R` + `ln -sf`.

### 5.2 CRLF: rompe `install.sh` **e** l'init script

`core.autocrlf=true` + nessun `.gitattributes` ⇒ nel working tree sia
`scripts/install.sh` sia `scripts/init.d/companion` hanno fine riga CRLF (i blob in git sono
LF: la conversione avviene in checkout e in export).

Sintomo su device:

```
: invalid optiong/extra/companion/install/scripts/install.sh: line 2: set: -
set: usage: set [-abefhkmnptuvxBCHP] [-o option-name] [--] [arg ...]
```

Il `\r` finale di `set -eu` viene passato come argomento a `set`; il ritorno carrello
sovrascrive l'inizio della riga a schermo, da cui l'output apparentemente corrotto.

**Attenzione al secondo file.** Sistemare solo `install.sh` non basta:
`install_init_script` copia `scripts/init.d/companion` in `/etc/init.d/companion`, e con
CRLF il servizio non parte — né a mano né al boot. Vanno normalizzati entrambi.

Fix immediato sul device:

```sh
cd <stage>
for f in scripts/install.sh scripts/init.d/companion; do
    tr -d '\r' < "$f" > "$f.lf" && mv "$f.lf" "$f"
done
```

Fix durevole nel repo — creare `.gitattributes`:

```gitattributes
* text=auto eol=lf
gst/** binary
```

Poi `git add --renormalize .` e ri-checkout. Senza questo, ogni clone su Windows
riproduce il problema.

### 5.3 I test non girano su Windows

`go test ./...` su Windows fallisce per motivi ambientali, non per bug del codice:

- `internal/media` non compila: `syscall.Setpgid` e `syscall.Kill` sono solo Linux
  ([audiobridge.go:393](../internal/media/audiobridge.go#L393), [:532](../internal/media/audiobridge.go#L532))
  — e con esso non compilano i test di `homekit`, `openwebnet`, `webui`, `diagnostics`;
- `fsync` su directory → `Access is denied` (test di `config` e `storage`);
- permessi `0600` non applicabili (test di `logging`);
- `/bin/tar` hardcoded (test di `system/update`).

Quello che **si può** verificare da Windows, e passa (exit 0) — type-check dell'intero albero,
file di test inclusi, per il target reale:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go vet ./...
```

Per eseguirli davvero serve Linux:

```sh
docker build --target test .    # lo stage `test` del Dockerfile fa `go test ./...`
wsl go test ./...               # oppure WSL
```

La CI li esegue su `ubuntu-latest` ad ogni tag `v*` ([release.yaml](../.github/workflows/release.yaml)).

---

## 6. Variabili d'ambiente di `install.sh`

Rilevanti solo in modalità download (senza argomento), tranne le ultime due:

| Variabile | Default | Uso |
|---|---|---|
| `COMPANION_RELEASE_REPO` | `r0bb10/BTicino-GO-Companion` | il fork `origin` è `abeilprincipino/…`: va impostata per installare dalle proprie release |
| `COMPANION_RELEASE_TAG` | *(vuota → latest)* | tag specifico |
| `COMPANION_RELEASE_BUNDLE_ASSET` | `companion.tar.gz` | nome asset |
| `COMPANION_RELEASE_BASE_URL` / `_API` | derivati da repo+tag | mirror o registry alternativo |
| `COMPANION_RELEASE_BUNDLE_SHA256` | *(dal digest dell'API GitHub)* | digest atteso, se l'API non è raggiungibile |
| `COMPANION_HEALTHCHECK_TIMEOUT_SEC` | `45` | vale **anche** in modalità locale |
| `COMPANION_SIP_INBOUND` | `1` | `0` salta il provisioning SIP in Flexisip e non tocca i file BTicino (§8); vale **anche** in modalità locale |

---

## 7. Note sul repo

Due incongruenze rilevate leggendo la pipeline, non bloccanti per l'install locale:

- **`dist/` non è in [.gitignore](../.gitignore)** (che copre solo `/companion`,
  `/coverage.out`, `/.dev/`), ma è la directory in cui scrivono sia il comando di §2 sia
  [release.yaml:32](../.github/workflows/release.yaml#L32). Gli artefatti di build finiscono
  in `git status`.
- **Il testo della release punta a un ref inesistente sul fork.** La `body` della release
  genera `.../raw/v3/scripts/install.sh` ([release.yaml:56](../.github/workflows/release.yaml#L56)),
  ma `origin` ha solo `main` — il branch `v3` esiste solo su `upstream`. Una release
  pubblicata da questo fork mostrerebbe un comando di install rotto.

---

## 8. Answering calls from Home Assistant

The installer provisions a `companion@<domain>` SIP user in Flexisip so the
intercom forks incoming calls to the companion. Run the installer with
`COMPANION_SIP_INBOUND=0` to skip this and leave the BTicino files untouched.

On the first start after an upgrade the companion adds the `companion.sip`
section to a `config.yaml` written before that section existed, with
`inbound: false`, so the installer has an `inbound:` key to enable. Nothing is
rewritten once the section is on disk.

**Flexisip is not restarted by the installer.** It reads
`/etc/flexisip/users/*` at startup, so after a successful provisioning run
(`OK: SIP inbound is provisioned.`) restart Flexisip or reboot the intercom
before the forked INVITE reaches the companion. Until then the card shows the
ring but no Answer button, exactly as if provisioning had not run.

> **Known limitation.** Logging out and back in from the BTicino DoorEntry app
> rewrites `/etc/flexisip/users/*` and removes the `companion` user. Answering
> then stops working **silently** — the card shows the ring but offers no Answer
> button. Re-run the installer to restore it. Backups of the three files are
> kept alongside them with a `.companion.bak` suffix.
