# Avvio dello stream: cosa è stato fatto e cosa si può ancora migliorare

Analisi del 4/11/2026 sul C100X, da `v0.13.2-beta.3` a `v0.13.3-beta.2`. Completa e in parte corregge
[2026-10-29-ring-snapshot-latency.md](2026-10-29-ring-snapshot-latency.md) (vedi §7).

## In breve

- **Oggi:** con la card il primo video arriva 4,1–4,6 s dopo l'offer, in LAN e in 5G. Prima la connessione
  WebRTC era pronta solo dopo 6,0–6,8 s.
- **Cosa resta:** dei 4,33 s misurati in 5G, circa 3,3 s sono di firmware e telecamera e non si toccano. Circa
  1 s è margine nostro o variabilità.
- **Obiettivo realistico:** 3,5–3,8 s. È una stima: nessun intervento del §3 è stato misurato.
- **Da dove partire:** `nice` su GStreamer e card che non aspetta microfono e Home Assistant prima dell'offer
  (§3.1 e §3.2). Costano poco. Il secondo può valere più di tutti, ma va prima misurato.
- **Aperti:** un riavvio spontaneo non spiegato, lo squillo non provato su questa versione, due test instabili
  (§5).

## 1. Dove vanno i 4,33 s

Vista in 5G delle 20:40:03. Il minimo viene dalle misure del §2: INVITE senza GStreamer per il totale fino al
200 OK, viste con Termix in pausa per le fasi interne. Le due fonti tornano: 0,09 + 0,70 + 1,95 = 2,74 s.

| Tratto | Vista in 5G | Minimo misurato | Di chi |
|---|---:|---:|---|
| Offer → INVITE | 0,13 s | ~0,02 s | companion |
| INVITE → 180 Ringing | 0,16 s | 0,09 s | firmware |
| 180 → chiamata sul bus (`*8*1#5#4#20*11##`) | 0,91 s | ~0,70 s | firmware e bus |
| Bus → 200 OK (accensione della telecamera) | 2,51 s | ~1,95 s | telecamera |
| 200 OK → primo video (avvio dell'encoder) | 0,62 s | ~0,54 s | firmware |
| **Totale** | **4,33 s** | **~3,3 s** | |

Il tratto più gonfio è l'attesa della telecamera, +0,56 s. Parte è probabilmente contesa di CPU con pipeline,
DTLS e candidati ICE, ma non è dimostrato: quel tratto varia anche da solo (1,39–2,29 s nelle viste RTSP).

## 2. Cosa è stato fatto e quanto ha reso

Il citofono ha **un solo core**: qualsiasi carico ritarda direttamente la gestione SIP del firmware.

Tempi dall'INVITE, mediane di viste RTSP (stessa partenza della sorgente usata dalla card, senza WebRTC):

| | Termix attivo (8) | Termix in pausa (8) | Avvio differito (8) | INVITE senza GStreamer (6) |
|---|---:|---:|---:|---:|
| 180 Ringing | 1,38 s | 0,95 s | 0,16 s | 0,09 s |
| 200 OK | 3,94 s | 3,61 s | 3,04 s | 2,74 s |
| Primo RTP | 4,70 s | 4,12 s | 3,65 s | non misurato |
| Primo RTP, min–max | 4,37–5,53 s | 4,06–4,56 s | 3,45–3,75 s | non misurato |
| Sorgente pronta | 4,99 s | 4,35 s | 3,84 s | non misurato |
| CPU libera (vista inclusa) | 55% | 84% | 86% | non misurata |

Con la card, sullo stesso firmware:

| | Prima (19:03–19:07, Termix attivo) | LAN, 2 viste (20:21–20:22) | 5G, 1 vista (20:40) |
|---|---:|---:|---:|
| Answer | 5,2–5,9 s | 0,07 s | 0,11 s |
| Connessione WebRTC pronta | 6,0–6,8 s | 0,28–0,37 s | 1,62 s |
| Primo pacchetto video | 4,8–5,7 s | 4,14–4,62 s | 4,33 s |

Le viste di prima e quella in 5G usano lo stesso tipo di percorso (`host/srflx`), quelle in LAN `host/host`, quindi
la fase ICE e DTLS delle viste in LAN non è del tutto confrontabile. La vista in 5G è durata 20 s: 1.531 pacchetti
video, 0 persi secondo il browser, nessuna richiesta di ritrasmissione o di keyframe.

**Le tre cause:**

1. **Termix, circa +0,6 s (la regressione).** Il plugin `host-metrics` teneva 3 loop SSH che ogni 2 s rileggevano
   tutto `/proc`. Sono comparsi quando è stato riaperto il terminale dopo il riavvio delle 18:46. Confronto a viste
   alternate, permutation test sul primo RTP: p=0,002.
2. **GStreamer prima dell'INVITE, circa 0,5 s recuperati (PR #11).** I 5 `gst-launch` (4 dell'audio bridge più il
   decoder dello snapshot) consumavano circa 0,9 s di CPU proprio mentre il firmware gestiva la chiamata. Ora
   partono al 180. Il massimo teorico era 0,87 s: restano circa 0,3 s perché continuano a occupare il core dopo
   il 180.
3. **ICE e DTLS in serie alla sorgente (PR #10, early answer).** Ora girano in parallelo all'avvio.

Il primo RTP con la card resta 0,4–0,9 s più tardi che con RTSP (4,06–4,54 s contro 3,65 s dall'INVITE). Due viste
non bastano a spiegarlo.

## 3. Miglioramenti possibili

I guadagni sono stime, non misure, e non si sommano. In ordine di convenienza.

### 3.1 Priorità bassa per GStreamer all'avvio

- **Idea:** far passare il firmware davanti alle nostre pipeline mentre si avviano. C'è margine: dal 180 al primo
  audio passano più di 3 s.
- **Dove:** `syscall.Setpriority` dopo `command.Start()` in `StartAudioBridge` (`internal/media/audiobridge.go`) e in
  `gstreamerSnapshotRunner.Start` (`internal/media/snapshot.go`).
- **Guadagno atteso:** 0,1–0,3 s, cioè parte dei ~0,3 s residui. **Sforzo:** basso.
- **Rischio:** a priorità bassa durante la chiamata l'audio può perdere pacchetti quando il core è occupato. Le
  pipeline audio vanno riportate a 0 quando la sorgente è partita. La priorità in Linux è per thread, quindi va
  applicata a tutti quelli in `/proc/<pid>/task`.
- **Misura:** 8 viste RTSP, confronto con la colonna "Avvio differito" del §2, poi una chiamata di 60 s per l'audio.

### 3.2 La card non aspetta microfono e Home Assistant prima dell'offer

Questo tempo è **fuori dai 4,33 s**, che partono dall'arrivo dell'offer al companion, e nessun log lo mostra.

- **Oggi:** `_start()` in `bticino-go-intercom-card.js` (repo Integration) fa in serie `getUserMedia`,
  `_fetchRtcConfiguration` (chiamata a HA), `createOffer`, `_subscribeRemoteCandidates` (chiamata a HA), e solo
  allora manda l'offer. In 5G ogni chiamata è un giro di rete in più.
- **Idea:** negoziare subito con `addTransceiver("audio", { direction: "sendrecv" })` e agganciare il microfono con
  `replaceTrack` quando arriva. Il percorso del preview fa già così. Tenere in cache la configurazione ICE dal
  caricamento della card.
- **Prima misurare:** `performance.now()` dal tocco all'offer e dalla connessione al primo frame. Se il tempo è
  piccolo, questo punto scende in fondo alla lista.
- **Guadagno atteso:** ignoto, probabilmente maggiore in 5G. **Sforzo:** basso.

### 3.3 Ultima foto visibile durante la connessione

- **Idea:** `_render()` nasconde l'immagine appena la vista è attiva. Tenerla fino al primo frame, con la scritta di
  connessione. L'attesa non si accorcia, ma lo schermo non resta vuoto.
- **Rischio:** una foto vecchia può sembrare dal vivo, quindi va segnalata come tale.
- **Sforzo:** basso.

### 3.4 Avvio differito anche sul ring

- **Idea:** sul ring le pipeline partono ancora a +0,1 s, in piena gestione della chiamata, perché `pipelineAV.StartVideo`
  le avvia subito. Spostarle al primo `*7*77` (~+0,35 s) o a ~+0,5 s. Il decoder non perde nulla: i pacchetti restano
  in coda dall'`Arm`.
- **Guadagno atteso:** foto, notifica e anteprima fino a ~0,5 s prima, se la contesa pesa come sulla vista. Non è
  dimostrato: oggi il primo RTP del ring è di solito a 2,9–3,2 s.
- **Sforzo:** basso, ma servono squilli veri. La riga `ring video timeline` del log dà già tutti i tempi.

### 3.5 Audio bridge da 4 a 2 processi

- **Idea:** è lo spunto S2 dell'analisi del 29/10: un processo per direzione, senza ricampionamenti.
- **Guadagno atteso:** 0,1–0,2 s all'avvio, circa metà del picco di CPU, e meno carico durante la chiamata.
- **Sforzo:** medio, serve il plugin `speex` nel GStreamer del bundle oppure PCMU verso il browser.

### 3.6 Pipeline audio sempre accese

- **Idea:** avviarle una volta sola e lasciarle ferme tra una vista e l'altra. Toglie tutto il picco dell'audio, anche
  sul ring. Il decoder dello snapshot esce dopo ogni foto, quindi andrebbe rilanciato dopo ogni vista a priorità bassa.
- **Guadagno atteso:** 0,3–0,5 s. È l'alternativa a §3.1 e §3.5, non si aggiunge a loro.
- **Sforzo:** medio-alto: ciclo di vita dei processi, porte UDP fisse occupate sempre, RAM da misurare.

### 3.7 Interventi minori

- **INVITE prima di elaborare l'offer:** `negotiateOffer` tiene ferma la sorgente per i 60–70 ms di
  `SetRemoteDescription`. Partire subito richiede di annullare lo stream se l'SDP è invalido. Massimo ~0,1 s.
- **Candidati relay solo se ICE non è ancora connesso:** 18 candidati su 26 nella vista in 5G erano relay, e il
  percorso scelto non li ha usati. Toglie lavoro mentre il firmware prepara la chiamata, ma chi ha bisogno di TURN si
  connette un po' dopo. Guadagno piccolo.
- **Audio insieme al video:** oggi l'audio si chiede dopo che il video scorre, con un controllo ogni 100 ms. Non
  cambia il primo frame, anticipa solo l'audio di 0,15–0,3 s.

## 4. Cosa non conviene

- **Tenere calda la telecamera:** accorcerebbe il tratto più lungo, ma attivarla in anticipo fa scattare il "tac" alla
  porta anche quando nessuno guarda.
- **Chiedere il video prima:** il primo RTP segue di 0,5–0,7 s lo stream dell'intercom (`*7*300#…#5002`), chiunque lo
  chieda. L'esperimento P4 del 31/10 lo ha già mostrato sul ring.

## 5. Problemi aperti

- **Riavvio spontaneo non spiegato.** Il 4/11 alle 20:26:45 il citofono si è riavviato da solo, circa un minuto dopo una
  vista in 5G di 2 s, con la build di prova dell'avvio differito. I log erano su tmpfs e sono persi, `pstore` è vuoto,
  nessun processo tiene aperto `/dev/watchdog`, e `bt_daemon` ha un proprio percorso di reboot. È successo 1 volta in 20
  viste, e 7 viste brevi non lo hanno ripetuto. Prima di quel giorno il citofono era acceso da 10 giorni. Non si può
  escludere la modifica. Per coglierne la causa i log vanno copiati fuori dal citofono mentre gira, perché un riavvio li
  cancella: `ssh citofono 'tail -n 0 -F /tmp/companion.log /var/log/log_rotation.log' > cattura.log`.
- **Squillo non provato** su `v0.13.3-beta.2`. L'ordine di avvio sul ring non è cambiato, ma va verificato.
- **Test instabili sul BYE di una chiamata risposta:** `TestStreamDialerOnByeEndsAnAnsweredInboundCall` (4/11) e
  `TestStreamDialerAnsweredInboundByeReleasesTheMediaLease` (3/11) sono falliti a caso in due release, e il BYE non
  arrivava al gestore. 300 esecuzioni locali sono passate. Non è indagato: può essere il test, o un problema reale sulle
  chiamate risposte.
- **Termix:** dopo un riavvio la nuova host key blocca il suo collector. Riaccettandola i loop probabilmente tornano,
  quindi conviene disattivare le metriche per il citofono nella sezione Host Metrics dell'host. I loop sono spariti tra
  le 19:51 e le 20:07 del 4/11 e non so perché.
- **`golangci-lint`** non è stato eseguito su PR #10 e #11: non è in CI né installato sul PC.

## 6. Come misurare

- **Una vista:** `ffmpeg -rtsp_transport tcp -i rtsp://<ip>:8554/doorbell-main -an -t 2 -f null -`. Non serve
  autenticazione. Ogni vista accende la telecamera, con il "tac" udibile.
- **Dove leggere i tempi:** `/tmp/companion.log` (`sip stream response`, `first valid packet`, `source session started`)
  e `/var/log/log_rotation.log`, dove le righe `<aswm>` hanno i microsecondi. Stanno entrambi su tmpfs.
- **Prima di confrontare:** controllare la CPU libera con due letture di `/proc/uptime` e cercare loop estranei con
  `grep -l "getconf PAGE[S]IZE" /proc/[0-9]*/cmdline`.
- **Spaziatura:** almeno 45 s tra le viste, e nessuna connessione SSH nuova nei 10 s prima: la sola negoziazione pesa
  sul core.
- **INVITE senza GStreamer:** INVITE SIP uguale a quello del companion su `127.0.0.1:5060`, poi ACK e BYE. Il companion
  scrive `WRN external stream detected`, ed è normale.
- **Da evitare:** rileggere tutto `/proc` ogni 200 ms prende circa il 50% del core e falsa le misure: il 180 dell'INVITE
  senza GStreamer è passato da 0,09 a 0,48 s.

## 7. Correzioni all'analisi del 29/10

- **Tempo "dell'intercom":** sulla live view circa 0,87 s attribuiti all'intercom erano nostri. Sul ring non è ancora
  misurato, quindi la frase "non pesano sul ring" del §1 resta da verificare.
- **§5 punto 3 (audio bridge):** il guadagno indicato di −87 ms misurava solo il lancio dei processi. Quello reale
  sulla live view è stato di circa 0,5 s.
- **§5 punto 2 (negoziare prima):** fatto con PR #10.
- **Registry di GStreamer:** il dubbio di una scansione dei plugin a ogni avvio è chiuso. Sistema e bundle usano cache
  separate e valide (`registry.arm.bin` e `registry.armv7l.bin`): avvio di 130–160 ms contro 800 ms di una scansione.
