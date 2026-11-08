# Avvio dello stream: contesa di CPU con il firmware

Analisi del 4/11/2026 sul C100X, da riprendere. Parte dai log già raccolti quel giorno: per scriverla non è stato aperto
nessuno stream. **Nulla di quanto proposto è stato implementato o misurato.**

Completa [2026-11-04-stream-start-improvements.md](2026-11-04-stream-start-improvements.md), che racconta cosa è stato
fatto e che resta valido salvo i punti del §7.

## In breve

- **Cosa resta:** con la card il primo RTP arriva 4,14–4,62 s dopo l'offer. Circa 3,4 s sono del firmware. Il margine è
  di circa 0,25 s su RTSP e 0,75–1,2 s sulla card: in gran parte contesa di CPU nostra, in parte non spiegato.
- **Limite del firmware:** il primo RTP arriva 2,5 s dopo `Call is an autoswitching`, a qualunque ora il firmware risponda
  alla chiamata SIP.
- **Due cause nuove:** i `gst-launch` a nice 0 passano davanti ai processi di chiamata del firmware, che sono a nice 10; e
  fino ad autoswitching la CPU resta a 396 MHz.
- **Da provare:** `gst-launch` a nice 19 fino al primo RTP, e governor `interactive` dall'inizio della vista. Si possono
  misurare entrambi senza toccare il codice (§8). L'obiettivo stimato per la card è 3,5–3,9 s.
- **Riavvio delle 20:26:** il registro del watchdog indica un timeout, non un'interruzione di corrente (§6).
- **Prossimo passo:** la misura del §8. Va concordata prima, perché ogni vista fa scattare il "tac" alla porta.

## 1. Dati

| Gruppo | Viste | Quando | Partenza del tempo |
|---|---:|---|---|
| Card | 3 (2 in LAN, 1 in 5G) | 20:21–20:22, 20:40 | Offer al companion |
| RTSP | 16 | 20:11–20:17, 20:41–20:44 | `source session starting` |
| INVITE senza GStreamer | 6 | 19:44–19:47 | INVITE |

- **Build:** card e RTSP hanno già l'avvio differito delle pipeline (PR #11, poi `v0.13.3-beta.2`), con log a livello
  `debug` e senza i loop di Termix.
- **Fonti:** `/tmp/companion.log` e `/var/log/log_rotation.log`. Stanno su tmpfs, quindi i dati vista per vista sono copiati
  in appendice.
- **Profili CPU:** due, delle 19:48 (vista con la build precedente) e delle 19:49 (INVITE senza GStreamer). Il campionatore
  occupa circa il 50% del core, quindi servono a vedere chi lavora e non a misurare i tempi.
- **Limite:** le viste con la card sono poche. Nessuna ha un profilo CPU.

## 2. Dove va il tempo

Mediane per RTSP e per l'INVITE senza GStreamer, intervallo per la card. In secondi.

| Tratto | Solo firmware | RTSP | Card |
|---|---:|---:|---:|
| Offer → INVITE | – | 0,01 | 0,08–0,13 |
| INVITE → 180 | 0,09 | 0,18 | 0,16–0,31 |
| 180 → `Call is an autoswitching` | 0,80 | 0,95 | 1,06–1,23 |
| Autoswitching → primo RTP | ~2,5 (stima) | 2,51 | 2,66–3,25 |
| **Totale** | **~3,4 (stima)** | **3,66** | **4,14–4,62** |

La stima nasce dal fatto che un INVITE senza GStreamer non chiede RTP: per quel tratto usa il valore delle viste RTSP. Il
documento del §7 arriva a ~3,3 s sommando le fasi in modo diverso, quindi il minimo è 3,3–3,4 s.

**Cosa fissa il primo RTP:**

- In 16 viste RTSP il primo RTP arriva 2,31–2,94 s dopo autoswitching, con mediana 2,51 s.
- In 8 chiamate su 50 il firmware risponde 0,55 s prima del solito, ma il primo RTP non si muove. Esempio delle 20:44:54:
  200 OK a 2,48 s invece di 3,03 s, primo RTP a 3,67 s, 2,55 s dopo autoswitching.
- Da autoswitching al passaggio dell'audio in `IP-EU` passano circa 2,1 s in entrambi i casi, 2,04–2,27 s nelle chiamate
  anticipate (kernel: `Audio Shark, new STATE = IP-EU`).
- Il kernel scrive `IPU Warning` circa 0,2 s prima del primo RTP: 0,17–0,23 s in 12 delle 13 viste che hanno quella riga.
  L'eccezione è la vista delle 20:21:47.

**Cosa fa il firmware in quei 2,5 s:** nei profili compaiono `Audio_State.sh`, `hbi_load_firmware` (programma il DSP audio
via SPI) e `bt_av_media`. Fino al 200 OK la CPU è libera per circa metà del tempo anche con il campionatore attivo, quindi è
in gran parte attesa. Non so quale passo la determini, né dove stia la telecamera in questa sequenza.

**Dove si gonfia la card:**

- **Prima di autoswitching:** ci arriva a 1,36–1,52 s dall'offer, contro 0,89 s del solo firmware.
- **Dopo autoswitching:** tutte e 3 le viste superano le viste RTSP normali (2,31–2,59 s). Non è spiegato.

## 3. Le due cause

### 3.1 Priorità invertite

| Nice | Processi |
|---:|---|
| 0 | `bt_av_media`, `scsserver`, `BtClass100`, `bt_vct`, **`companion` e i suoi `gst-launch`** |
| 10 | `bt_answering_machine` (le righe `<aswm>`), `flexisip`, `openserver`, `bt_eliot` |

- Il firmware assegna questi valori da sé: `bt_daemon` contiene `nice` e `chrt` e avvia con `start-stop-daemon --chuid
  bticino:bticino`. Il companion è a 0 solo per default: né il codice né `scripts/init.d/companion` lo impostano.
- Il `nice` conta davvero: il kernel è un 4.9 senza `CONFIG_CGROUP_SCHED` né `CONFIG_SCHED_AUTOGROUP`, e nessun processo del
  firmware usa classi in tempo reale. Tra i thread del kernel lo fa `ipu1_task` (`SCHED_FIFO`).
- I pesi di CFS sono 1024 a nice 0, 110 a nice 10 e 15 a nice 19. Nei profili le 5 pipeline in avvio lavorano insieme e
  occupano il core per circa 0,9 s di CPU. In quel tempo, a un processo a nice 10 resta:

| `gst-launch` a nice | CPU al processo a nice 10 |
|---:|---:|
| 0 (oggi) | 2% |
| 5 | 6% |
| 10 | 17% |
| 19 | 59% |

Può spiegare anche una misura rimasta strana: un carico del 50% portava il 180 dell'INVITE senza GStreamer da 0,09 a
0,48 s. Il 180 passa da due processi a nice 10.

### 3.2 CPU al minimo fino ad autoswitching

- **Comportamento di fabbrica:** il default del kernel è `powersave`. `/etc/init.d/bt_daemon-apps.sh` dice "boot is done in
  powersave to avoid large sinkings" e passa a `interactive` solo con l'alimentazione ausiliaria (`gpio47=0`). Questo
  citofono ha `gpio47=1`, quindi a riposo sta a 396 MHz. Un'unità con alimentazione ausiliaria può comportarsi diversamente,
  e va tenuto presente nei confronti.
- **Cosa succede in chiamata (dedotto, mai visto direttamente sul file):**
  - Nei due profili il tempo CPU del campionatore scende di circa 2,5 volte proprio ad autoswitching e compare il thread
    `cfinteractive`. Nel profilo dell'INVITE quel tempo risale circa 1,5 s dopo il BYE.
  - `time_in_state` dava 62 s sopra 396 MHz contro 58 s di chiamate dal boot, e a riposo quei contatori non si muovono.
- **Chi lo cambia (dedotto):** `bt_answering_machine`. È l'unico dei 7 binari controllati a importare `hal_set_cpu_speed` da
  `liblghal.so`, che contiene i percorsi e le stringhe `interactive` e `powersave`. Gira come utente `bticino`, e lo script fa
  `chmod 666` su un file che altrimenti quell'utente non potrebbe scrivere.
- **Effetto:** le 5 pipeline partono al 180, quindi quasi tutto il loro avvio cade a 396 MHz, insieme a SDP, ICE e DTLS.

## 4. Cosa si può fare

I guadagni sono stime, si sovrappongono e non si sommano. In ordine di convenienza.

### 4.1 `gst-launch` a nice 19 fino al primo RTP

- **Idea:** cedere la CPU al firmware durante l'avvio. A core libero un processo a nice 19 corre a piena velocità, e le
  pipeline hanno più di 3 s di margine prima del primo RTP.
- **Come:** `syscall.Setpriority(syscall.PRIO_PROCESS, pid, 19)` subito dopo `command.Start()`, in
  `StartAudioBridge` (`internal/media/audiobridge.go`) e in `gstreamerSnapshotRunner.Start` (`internal/media/snapshot.go`). I
  thread creati dopo ereditano il valore. In alternativa si antepone `/usr/bin/nice -n 19` al comando, che non ha la minima finestra iniziale a nice 0 ma
  dipende dalla presenza del programma sull'unità.
- **Ritorno a 0:** quando il flusso è partito, su ogni thread in `/proc/<pid>/task`, perché `nice` in Linux è per thread. Il
  punto naturale è `pipelineAV` in `internal/app/pipelines.go` (su `develop`): `SourceAV.Start` e `StartVideo` tornano quando
  il flusso è partito o è fallito. Il companion gira come root e può rialzare la priorità. Il ritorno serve: in streaming il
  core è molto carico e l'audio potrebbe perdere pacchetti.
- **Guadagno atteso:** fino a 0,25 s su RTSP e buona parte dei ~0,5 s prima di autoswitching sulla card.
- **Rischio:** nessun effetto sul consumo. L'audio va riascoltato con una chiamata di 60 s.

### 4.2 Governor `interactive` dall'inizio della vista

- **Idea:** il companion scrive `interactive` in `/sys/devices/system/cpu/cpufreq/policy0/scaling_governor` all'offer,
  all'avvio della sorgente o al primo frame dello squillo, circa 1 s prima del firmware. A fine chiamata lo riabbassa il
  firmware. Se la chiamata non parte, lo riabbassa il companion.
- **Guadagno atteso:** circa 0,3 s su RTSP e 0,5 s sulla card. Oltre a togliere la contesa, accelera anche i passi del
  firmware prima di autoswitching, circa 0,1 s (quindi il minimo scenderebbe a ~3,3 s). L'avvio di GStreamer legge anche da
  flash, quindi non accelera di tutte le 2,5 volte. Dopo il §4.1 resterebbero al massimo 0,15 s.
- **Rischio:** vedi sotto. L'incognita è la CPU al massimo mentre si accende il posto esterno, circa 0,7 s che oggi il
  firmware passa a 396 MHz. Non so se sia voluto.

**Perché non tenerlo sempre in `interactive`.** La resa sulla vista è la stessa, e costa di più:

| | Sempre | Dall'inizio della vista |
|---|---|---|
| Come | Scrittura più `chmod 644` dopo ogni boot, altrimenti il firmware lo rimette in `powersave` a fine chiamata | Una scrittura del companion |
| Stand-by | Picchi in più sul bus | Nessun cambiamento |
| Firmware | Lo scavalca: le sue scritture fallirebbero | Nessun conflitto |

**Consumi:**

| Dato | Valore | Origine |
|---|---:|---|
| Massimo dal bus in stand-by | 37 mA, circa 1 W a 27 V | Scheda BTicino 344682 |
| Massimo dal bus in funzionamento | 400 mA | Scheda BTicino 344682 |
| Tensione del core a 396 / 792 / 996 MHz | 1,15 / 1,175 / 1,25 V | Device tree |
| Potenza del core a 996 MHz rispetto a 396 | circa 3 volte, mentre lavora | Calcolo (f·V²) |
| Energia per istruzione a 996 MHz rispetto a 396 | +18% | Calcolo (V²) |
| Carico a riposo | circa 2,5% | `/proc/stat`, 10 s |
| Picco in più sul bus | circa +15 mA | Stima da valori tipici dell'i.MX6 |

A riposo la media cambierebbe poco, ma i picchi sono molti rispetto a 37 mA. Durante una vista siamo già "in funzionamento",
dove pesano poco rispetto a 400 mA. Il citofono non ha sensori di corrente o di tensione del bus: `/sys/class/power_supply` e
`/sys/bus/iio/devices` sono vuoti, e `hwmon0` è solo la temperatura (40–45 °C a riposo, primo limite a 85 °C).

### 4.3 Il companion intero a nice 10

- **Pro:** è un ospite e somiglia ai processi a nice 10. Oggi un suo picco di CPU passa davanti alla gestione delle chiamate.
  Basta `/usr/bin/nice -n 10` davanti a `start-stop-daemon` in `scripts/init.d/companion`, e i `gst-launch` lo ereditano.
- **Contro:** vale sempre, anche per l'inoltro dell'RTP e per l'audio, mentre `bt_av_media` resta a 0. Da solo non basta: al
  firmware resterebbe il 17%, contro il 59% del §4.1.
- **Parere:** il processo Go in streaming usa il 5–15% del core, e sulle viste RTSP non è il problema. Lo porterei a 10 solo
  dopo aver verificato che non perde pacchetti. Sulla card può contare di più, perché DTLS e candidati girano a nice 0.

### 4.4 Avviare le pipeline al primo RTP video

Alternativa al §4.1: toglie tutto il loro lavoro dal tratto del firmware, senza cambiare priorità, e a quel punto la CPU è già
veloce. Però l'audio parte più tardi (stima 0,3–0,5 s) e le pipeline contendono il core al primo secondo di video. Il commento
in `internal/app/pipelines.go` dice che dopo il 180 il firmware "mostly waits": in realtà in quel tratto gestisce anche la
chiamata sul bus, a 396 MHz.

### 4.5 Altri

- **Card e offer:** `_start()` in `bticino-go-intercom-card.js` (repo Integration) aspetta microfono, configurazione ICE e
  sottoscrizione dei candidati. È il §3.2 del documento citato, fuori dai 4,3 s. Va prima misurato.
- **Certificato DTLS:** `rtcConfiguration` non passa `Certificates`, quindi pion genera chiave ECDSA e certificato a ogni
  `NewPeerConnection`, a 396 MHz e prima di `have-remote-offer`. Generarlo una volta all'avvio è semplice. Guadagno piccolo e
  non misurato, anche questo fuori dai 4,3 s.

## 5. Cosa non conviene

- **Sperare nella risposta anticipata del firmware:** non anticipa il primo RTP (§2).
- **Chiedere il video prima:** l'ACK arriva già in 5–10 ms, ma il video parte comunque dopo l'avvio dell'IPU.
- **Fare l'INVITE prima di elaborare l'offer, da solo:** il lavoro del companion resta a nice 0 e passa comunque davanti ai
  primi passi del firmware. Può servire solo dopo il §4.1 o riducendo quel lavoro.
- **Telecamera calda:** a parte il "tac", una vista 6,9 s dopo la precedente (20:22:02) non è stata più veloce nei passi del
  firmware, e nemmeno quelle a 26 s invece che a 43 s.
- **Modificare il firmware:** i 2,5 s includono la riprogrammazione del DSP in `Audio_State.sh`. Toccarlo vuol dire
  cambiare file del sistema, per un guadagno ignoto e con il rischio di rompere l'audio dell'app ufficiale.

## 6. Il riavvio delle 20:26:45

Letture in sola lettura con `devmem2`, dopo il riavvio:

| Registro | Valore | Significato |
|---|---:|---|
| `WDOG1_WRSR` (`0x020BC004`) | `0x0002` | Ultimo reset da timeout del watchdog; bit di power-on a 0 |
| `WDOG1_WCR` (`0x020BC000`) | `0x7735` | Watchdog attivo, timeout di 60 s |
| `SRC_SRSR` (`0x020D8008`) | `0x00000000` | Nessuna informazione: probabilmente azzerato al boot |

- **Cosa suggerisce:** il sistema si è bloccato per circa un minuto, e non ha perso corrente. Il riavvio è arrivato "circa un
  minuto dopo una vista in 5G di 2 s", quindi il blocco potrebbe essere nato attorno alla fine di quella vista.
- **Chi tiene vivo il watchdog:** nessun processo ha `/dev/watchdog` aperto. Lo aggiorna presumibilmente il thread del kernel
  `watchdogd`, a nice −20. Per farlo scadere serve un blocco nel kernel o un thread in tempo reale che non molla il core, come
  `ipu1_task`. Sono ipotesi.
- **Perché non resta traccia:** `CONFIG_LOCKUP_DETECTOR` è spento, e `pstore` è vuoto sia in `/sys/fs/pstore` sia in
  `/var/tmp/pstore`.
- **Cosa non è escluso:** un calo di tensione che blocca la CPU senza riavviarla. Inoltre il valore non è stato confrontato con
  quello di un riavvio normale, che su questo kernel potrebbe usare lo stesso meccanismo. Va controllato alla prima occasione,
  leggendo `WRSR` dopo un `reboot`.

## 7. Differenze rispetto a 2026-11-04-stream-start-improvements.md

- **§1, "accensione della telecamera":** il tratto da ~1,95 s non è dimostrato come tempo di telecamera (§2 qui sopra).
- **§3.1:** il meccanismo è più forte di una generica contesa, perché i processi di chiamata sono a nice 10.
- **§3.7, INVITE prima dell'offer:** il guadagno è probabilmente minore di ~0,1 s, se non cambia la priorità (§5 qui sopra).
- **§5, riavvio:** ora c'è un'indicazione sulla causa (§6 qui sopra).
- **Non trattato lì:** il governor.

Quel documento non è stato modificato.

## 8. Come riprendere: la misura

Non servono modifiche al codice. Vanno rispettate le regole del §6 di quel documento: almeno 45 s tra le viste, nessuna nuova
connessione SSH nei 10 s prima, CPU libera, nessun loop estraneo.

**Prima di partire:** concordare la prova. Salvare i log prima di ogni riavvio del companion. A fine prova verificare che
nessuno script sia rimasto attivo e che nel log del citofono non compaiano altre viste.

```sh
# Una vista (ogni vista accende la telecamera, con il "tac")
ffmpeg -rtsp_transport tcp -i rtsp://<ip>:8554/doorbell-main -an -t 2 -f null -

# A. Riferimento: stessa sequenza, senza cambiare nulla
ssh citofono true; sleep 12; <vista>; sleep 45

# B. Governor: conferma anche che il firmware lo riabbassa da solo
ssh citofono 'echo interactive > /sys/devices/system/cpu/cpufreq/policy0/scaling_governor'; sleep 12; <vista>; sleep 45
ssh citofono 'cat /sys/devices/system/cpu/cpufreq/policy0/scaling_governor'   # atteso: powersave

# C. Nice: tutti i thread del companion a 19, i gst-launch lo ereditano
ssh citofono 'for t in /proc/$(pidof companion)/task/*; do renice 19 -p ${t##*/}; done'; sleep 12; <vista>; sleep 45

# Ripristino dopo C (oppure riavviare il companion)
ssh citofono 'for t in /proc/$(pidof companion)/task/*; do renice 0 -p ${t##*/}; done'
```

Il caso C abbassa anche il processo Go, quindi dà il massimo che ci si può aspettare dal `nice`, non il §4.1 esatto. Basta per
decidere, ma non dice nulla sull'audio. Il ripristino va fatto subito dopo ogni vista C.

**Quante viste:** circa 15 RTSP, 5 per caso, alternate. Poi le stesse condizioni con la card, da aprire a mano: è lì che il
guadagno atteso è maggiore e che i dati sono meno.

**Cosa confrontare:**

- **Dal log del companion:** `sip stream response` (180 e 200), `rtp receiver received first valid packet`.
- **Da `/var/log/log_rotation.log`:** `Call is an autoswitching`, `Answering sip call`, `Audio Shark, new STATE`.
- **Confronto:** tratti del §2 con le tabelle dell'appendice. Il primo RTP cambia poco di suo: RTSP 3,45–3,77 s, con
  due viste a 4,01 s.

**Come leggere il risultato:**

| Esito | Conclusione |
|---|---|
| C porta RTSP vicino a 3,4 s | Implementare il §4.1 e controllare l'audio con una chiamata di 60 s |
| B fa meglio di C di almeno 0,1 s | Valutare il §4.2, tenendo conto del rischio sul bus |
| Né B né C muovono il tratto prima di autoswitching | Le cause del §3 vanno riviste |
| La card resta sopra RTSP dopo autoswitching | Serve un profilo CPU di una vista dalla card |

**Dopo:** una chiamata di 60 s per l'audio, e uno squillo vero, non ancora provato su questa versione.

## 9. Domande aperte

- Perché la card resta sopra RTSP anche dopo autoswitching, di 0,15–0,75 s?
- Perché 2 viste su 19 hanno il primo RTP in ritardo di 0,35–0,5 s (20:21:47 con due `IPU Warning`, 20:42:44 con
  l'IPU tardivo)? Sembra lato firmware, ma non so se la contesa c'entri.
- Quale passo del firmware fissa i 2,5 s dopo autoswitching?
- Quanto assorbe davvero dal bus questa unità a 396 e a 996 MHz? Serve uno strumento esterno.
- Sullo squillo, quando passa a `interactive` il firmware? Il momento è noto solo per le viste.
- Cosa ha bloccato il sistema alle 20:25–20:26?

## Fonti esterne

- [Scheda tecnica Classe 100X16E](https://dar.bticino.it/asset/Documents/ST-00002025-IT.pdf)
- [Manuale LE10815AC](https://assets.legrand.com/pim/NP-FT-GT/LE10815AC.pdf)

## Appendice: dati vista per vista

Secondi. Le viste RTSP partono da `source session starting`, e l'INVITE esce 0,00–0,02 s dopo.

**RTSP (16):**

| Ora | 180 | `*8*1` | Autoswitching | 200 OK | Primo RTP | Autoswitching → RTP |
|---|---:|---:|---:|---:|---:|---:|
| 20:11:41 | 0,14 | 0,83 | 1,12 | 3,06 | 3,69 | 2,56 |
| 20:12:31 | 0,21 | 0,92 | 1,15 | 3,03 | 3,65 | 2,50 |
| 20:13:21 | 0,13 | 0,81 | 1,16 | 3,10 | 3,75 | 2,59 |
| 20:14:11 | 0,18 | 0,93 | 1,13 | 3,01 | 3,62 | 2,49 |
| 20:15:01 | 0,13 | 0,84 | 1,09 | 2,96 | 3,46 | 2,36 |
| 20:15:51 | 0,10 | 0,85 | 1,10 | 3,00 | 3,61 | 2,51 |
| 20:16:41 | 0,21 | 0,92 | 1,17 | 3,04 | 3,66 | 2,49 |
| 20:17:31 | 0,20 | 0,87 | 1,16 | 3,08 | 3,74 | 2,57 |
| 20:17:54 | 0,22 | 0,96 | 1,14 | 3,03 | 3,65 | 2,52 |
| 20:41:41 | 0,20 | 0,89 | 1,19 | 3,15 | 3,77 | 2,58 |
| 20:42:13 | 0,21 | 0,91 | 1,14 | 3,02 | 3,45 | 2,31 |
| 20:42:44 | 0,15 | 0,84 | 1,07 | 2,98 | 4,01 | 2,94 |
| 20:43:18 | 0,42 | 1,18 | 1,51 | 3,49 | 4,01 | 2,49 |
| 20:43:50 | 0,20 | 0,93 | 1,15 | 3,03 | 3,56 | 2,42 |
| 20:44:22 | 0,18 | 0,94 | 1,16 | 3,06 | 3,68 | 2,52 |
| 20:44:54 | 0,15 | 0,89 | 1,12 | 2,48 | 3,67 | 2,55 |

**Card (3), dall'offer:**

| Ora | Rete | INVITE | 180 | ICE | DTLS | `*8*1` | Autoswitching | 200 OK | Primo RTP | Autoswitching → RTP |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 20:21:47 | LAN | 0,10 | 0,30 | 0,19 | 0,37 | 0,95 | 1,36 | 3,44 | 4,62 | 3,25 |
| 20:22:02 | LAN | 0,08 | 0,39 | 0,11 | 0,28 | 1,20 | 1,48 | 3,47 | 4,14 | 2,66 |
| 20:40:03 | 5G | 0,13 | 0,29 | 0,76 | 1,62 | 1,20 | 1,52 | 3,71 | 4,33 | 2,81 |

**INVITE senza GStreamer (6), dall'INVITE:**

| Ora | 180 | 200 OK |
|---|---:|---:|
| 19:44:06 | 0,19 | 2,88 |
| 19:44:50 | 0,08 | 2,74 |
| 19:45:34 | 0,10 | 2,74 |
| 19:46:17 | 0,10 | 2,74 |
| 19:47:01 | 0,09 | 2,66 |
| 19:47:45 | 0,08 | 2,78 |

**Passi del firmware**, mediane da `log_rotation.log`. "Nuova chiamata" è `SIP: New incoming call`, "dal bus" è `New incoming call
from scs`. L'ultima riga esclude le chiamate con risposta anticipata:

| Tratto | Solo firmware | RTSP | Card |
|---|---:|---:|---:|
| Nuova chiamata → dal bus | 0,68 | 0,79 | 0,78–0,93 |
| Dal bus → autoswitching | 0,12 | 0,18 | 0,22–0,31 |
| Autoswitching → `Answering sip call` | 1,85 | 1,89 | 1,98–2,18 |

Da autoswitching, con risposta normale `Answering sip call` arriva a ~1,85 s, nelle 8 chiamate anticipate a 1,27–1,42 s.
