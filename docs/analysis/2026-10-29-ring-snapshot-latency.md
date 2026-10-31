# Latenza ring → snapshot → notifica Home Assistant

Analisi del 2026-10-29 su companion `v0.11.0-beta.11` (`main` @ `a3e9442`) e integrazione `v3` @ `27b0d90`.
Fonte: log DEBUG del ring delle 09:46:15 (non risposto, porta aperta dalla card) e della live view
on-demand delle 09:46:45, più 10 ring del 28/10 ripresi dai log grezzi della sessione precedente (§1.2). Le
affermazioni ereditate da analisi precedenti e non riverificate sui log sono segnalate come tali. Il §8
confronta il companion con ha-bticino-c300x, un progetto analogo per il C300X. Il §9 riporta le prove del
31/10 (v0.12.1-beta.1 → beta.8): P4, P2, S1, S3, §5 punto 7, S8 e le verifiche fatte via SSH sul citofono.

## In breve

- Lo snapshot del ring viene pubblicato **3,27 s** dopo il frame OpenWebNet `ring.started`. **3,11 s** sono
  dell'intercom (accensione della telecamera del posto esterno e avvio del suo encoder) e il companion non li
  accorcia con i mezzi provati finora (§1.1). Il companion aggiunge **0,16 s**, dal primo pacchetto RTP al JPEG.
- **Il NACK della prima richiesta non è la causa del secondo perso** (§1.2, 11 ring): arriva insieme al rilancio
  dell'attivazione video da parte dell'intercom (`*7*58`), e l'ACK successivo coincide con l'avvio degli stream
  dell'intercom stesso. **P4 lo ha confermato il 31/10** (§9.1): senza nessuna nostra richiesta l'intercom
  rilancia comunque l'attivazione a +1,1–1,6 s e avvia i suoi stream negli stessi tempi di prima. La richiesta
  reattiva resta in uso perché è più pulita (ACK in 3–13 ms, niente NACK né retry), ma non accorcia la foto.
- **P2 provato sul device** (§9.2): l'encoder JPEG del VPU è più rapido a riposo ma più lento durante il ring,
  per la contesa del VPU con l'intercom. Resta `jpegenc` a `quality=80` (JPEG da ~58 a ~40 KB).
- Il ritardo della notifica nasce dall'architettura, non dalla pipeline: il README dell'integrazione fa partire
  la notifica dal cambio dell'entità `image` *last doorbell ring*, che per costruzione cambia solo a ~3,3 s.
  Il segnale istantaneo esiste già (`binary_sensor` *Ringing*, acceso a T0 + pochi ms) ma non viene usato.
- **Leva principale (P0, nessuna release):** notifica in due fasi. Si avvisa subito al ring, poi la stessa
  notifica (stesso `tag`) viene aggiornata in silenzio con la foto. L'avviso arriva in ~0,3–1 s invece di ~4–5 s.
- Nel companion si recuperano ~60–100 ms sullo snapshot (P2) e ~0,5 s sull'avvio della live view on-demand (§5).
- C'è un bug: **HomeKit allega la foto del ring precedente** (P3).
- Sullo stream (§6): un **bug latente nell'audio bridge** (i pacchetti in coda condividono il buffer di
  lettura), un audio bridge che può passare da 4 processi a 2 senza ricampionamenti, lo snapshot che clona ogni
  pacchetto video per tutta la durata dello stream, e una **live view chiusa dall'intercom dopo ~31,6 s** pur
  con il keepalive attivo. La causa era il keepalive in chiaro su una chiamata SRTP, e valeva anche per le
  chiamate risposte. Con il keepalive SRTP (S8, beta.8) la live view dura 60 s (§9.4): per ora basta, e il
  limite dei 60 s resta come spunto (S9).
- ha-bticino-c300x (§8) non ha una strada più rapida per il video del ring. Ne vengono due spunti: il keepalive
  SRTP (S8) e l'audio in PCMU, che toglie Opus e ricampionamenti dal percorso WebRTC (S2).

## 1. Dove va il tempo

Ring delle 09:46:15.842 (T0 = frame `*8*1#1#4#21*11##`, `ring.started`):

| t da T0 | Evento (log) | Δ | Di chi |
|---:|---|---:|---|
| 0,000 | `ring.started` | | intercom |
| +0,099 | `inbound sip invite received` | 99 ms | intercom |
| +0,101 | `stream lease acquired ringing_preview=true` (ring holder) | 2 ms | companion |
| +0,113 → +0,173 | audio bridge: 4 processi `gst-launch` avviati in serie | 60 ms | companion |
| +0,174 | richiesta AV video n. 1 (`*7*300` su :30007) | | companion |
| +0,375 | `*7*77` (parametri video del posto esterno) | | intercom |
| +1,574 | risposta n. 1: **NACK** dopo 1,40 s | 1 400 ms | intercom |
| +1,680 | richiesta n. 2 (retry dopo 100 ms) | 106 ms | companion |
| +2,446 | risposta n. 2: **ACK** dopo 766 ms | 766 ms | intercom |
| +2,494 | l'intercom configura il proprio stream per il display (`*7*300#127#0#0#1#5002#1`) | 48 ms | intercom |
| +3,112 | primo pacchetto RTP H.264 (AUD+SPS+PPS+IDR) | 666 ms dall'ACK | intercom |
| +3,144 | IDR completo (`ring clip first keyframe`) | 32 ms | intercom/rete |
| +3,267 | `snapshot captured` (`decode=121ms`, `fed_access_units=1`) | 123 ms | companion |
| ≈ +3,27 | `BroadcastState` → HA → l'entità `image` cambia stato | pochi ms | companion/HA |
| +3,3 → +5 (stima) | automazione → push FCM/APNs → il telefono scarica `/api/image_proxy/…` | 0,5–2 s | HA/telefono |

Fino al JPEG: intercom 3,11 s (95 %), companion 0,16 s (5 %). I 60 ms dell'audio bridge non pesano sul ring
(vedi §1.1, punto 2) ma pesano sulla live view on-demand (§5).

### 1.1 Perché finora la parte dell'intercom non si è accorciata

1. **Il NACK della prima richiesta non anticipa il video.** In 9 ring su 11 arriva a +1,53–1,63 s (§1.2).
   Secondo l'analisi precedente arriva lì anche quando la richiesta parte dopo `*7*77` (beta.10, revert in
   `a3e9442`), ma i log di beta.10 non li ho ritrovati: il dato è da riconfermare.
2. **L'ACK coincide con il momento in cui l'intercom ha il video pronto, non con il retry.** 48 ms dopo il
   nostro ACK l'intercom configura il proprio stream per il display. Nella live view on-demand delle 09:46:45,
   dove non c'era nessuna nostra richiesta AV in attesa, l'intervallo `*7*77` → stream del display vale
   1,94 s (nel ring 2,12 s). È il tempo di accensione della telecamera e del canale video sul bus, e c'è anche
   quando non chiediamo niente.
3. **Dallo stream configurato al primo RTP passano 0,62 s nel ring e 0,64 s on-demand.** È l'avvio
   dell'encoder dell'intercom, uguale chiunque lo chieda.
4. **Non esiste un flusso precedente da intercettare.** Lo stream del display (127.0.0.1:5002) parte nello
   stesso istante del nostro e con lo stesso SSRC (probe di loopback di beta.8: dato dell'analisi precedente,
   non riverificato qui).

Con il comportamento attuale dell'intercom, quindi, **la prima immagine arriva a ~T0+3,0–3,1 s** (con due
eccezioni più rapide, §1.2). Avviare lo stream dal frame OpenWebNet invece che dall'INVITE (−99 ms), togliere
il retry delay (−100 ms) o l'audio bridge (−60 ms) anticiperebbe solo la richiesta, non l'ACK.

### 1.2 Il NACK della prima richiesta: cosa dicono 11 ring

Un'analisi precedente attribuiva al NACK della prima richiesta circa 1 s di ritardo. Ho rimesso in fila i ring
del 28/10 (log della sessione beta.6–beta.10) e quello di oggi. Tempi in secondi da `ring.started`:

| Ring | richiesta n. 1 | `*7*77` | NACK | ACK | stream dell'intercom (5002) | primo RTP |
|---|---:|---:|---:|---:|---:|---:|
| 28/10 18:40:02 | +0,19 | +0,33 | +1,58 | +2,48 | +2,49 | +3,10 |
| 28/10 18:47:15 | +0,19 | +0,37 | +1,53 | +2,40 | +2,42 | +2,96 |
| 28/10 18:59:46 | +0,20 | +0,39 | +1,61 | +2,46 | +2,49 | +3,01 |
| 28/10 19:13:45 | +0,19 | +0,33 | +1,61 | +2,45 | +2,47 | +3,06 |
| 28/10 19:53:53 | +0,18 | +0,33 | +1,54 | +2,37 | +2,42 | +3,08 |
| 29/10 09:46:15 | +0,17 | +0,38 | +1,57 | +2,45 | +2,49 | +3,11 |
| 28/10 18:40:15 (a) | +0,15 | +0,15 | +0,53 … +1,87 | +2,71 | +2,76 | +3,25 |
| 28/10 18:40:37 (b) | +0,33 | +0,34 | +1,07 | +1,38 | +1,44 | +2,33 |
| 28/10 19:00:00 (b) | +0,34 | +0,54 | +0,87 | +1,85 | +1,91 | +2,50 |

(a) Squillo arrivato 13 s dopo il precedente, mentre quella chiamata era probabilmente ancora attiva: tre NACK
immediati, poi uno trattenuto. (b) Squilli arrivati 14–22 s dopo il precedente. Altri due ring del 28/10 (13:25
e 16:51), registrati senza i frame AV, seguono lo schema normale: ACK a +2,47 e +2,43, primo RTP a +3,06 e
+2,94.

Cosa ne segue:

1. **Il NACK arriva sempre insieme a un `*7*58#8#0#0#1` dell'intercom** (entro 6 ms), lo stesso frame che
   l'intercom emette a +0,01–0,03 s da ogni ring. L'intercom rilancia l'attivazione del video e scarta la
   richiesta in attesa.
2. **Dopo il rilancio il video è pronto in 0,86–0,91 s**: ACK e stream dell'intercom arrivano insieme, entro
   ~50 ms. Il primo RTP segue dopo altri 0,5–0,7 s.
3. **Un retry più rapido, o una richiesta mai rifiutata, avrebbe ricevuto l'ACK nello stesso istante**, perché
   l'ACK coincide con l'avvio degli stream dell'intercom stesso. Il NACK è il sintomo del rilancio, non la sua
   causa.
4. **Nelle live view on-demand non c'è nessun `*7*58`**, e da `*7*77` agli stream dell'intercom passano
   1,94–1,96 s; nei ring normali 2,06–2,17 s. Se il percorso del ring è intrinseco, il rilancio costa al
   massimo ~0,15 s.
5. **Resta un'ipotesi da escludere:** che sia la nostra richiesta, inviata a +0,17 s mentre l'intercom attiva il
   video, a far fallire il primo tentativo. Ritardarla a dopo `*7*77` (beta.10) non è bastato, ma `*7*77` cade
   ancora dentro la finestra di attivazione. Se l'ipotesi è vera, senza richiesta anticipata il video partirebbe
   senza rilancio, potenzialmente fino a ~1 s prima. Lo verifica P4.
6. I due ring più rapidi (b) sono arrivati poco dopo un ring precedente: compatibile con una telecamera del posto
   esterno ancora calda. Il companion non può controllarlo.

## 2. La catena della notifica oggi

1. Frame `ring.started` → `Listener.Run` → `newEventApplier` → `Projector` (`PhysicalRing`,
   `call_state=ringing`) → `BroadcastState` sul WebSocket ([run.go:599](../../internal/app/run.go#L599)).
   In HA il `binary_sensor` *Ringing* va a `on` a T0 + pochi ms.
2. INVITE SIP → `IncomingCallStarted` → `ringHolder.hold` → `AcquireRingingPreview` → `newBridgeSource`, che
   arma lo snapshot e avvia subito `gst-launch … imxvpudec ! jpegenc` → audio bridge → `SourceSession.Start`
   → `AVClient.StartVideo` ([ringholder.go:64](../../internal/app/ringholder.go#L64),
   [run.go:627](../../internal/app/run.go#L627)).
3. Primo RTP → `snapshotAttempt.Consume` → access unit dall'IDR in poi → decoder → `publish`, che scrive
   `snapshots/<id>.jpg` e `snapshots/ring/<id>.jpg` (file temporaneo + rename, sotto lock) e chiama
   `onCaptured` = `BroadcastState` ([snapshot.go:419](../../internal/media/snapshot.go#L419)).
4. HA riceve `ring_snapshot_updated_at` (mtime del file) e l'entità `CompanionRingSnapshotImage` cambia stato
   (`image.py:80-84` nell'integrazione).
5. L'automazione del README (trigger sull'entità `image`) chiama `notify.mobile_app_*` con
   `image: /api/image_proxy/image.…`.
6. Il telefono riceve il push e scarica l'immagine da HA. Solo a quel punto HA esegue `async_image()` e
   scarica il JPEG dal companion (`GET /api/v3/entrypoints/{id}/snapshot/ring.jpg`): il fetch è lazy e sta sul
   percorso critico (`image.py:86-99`).

Conseguenze:

- il tempo percepito è ~3,3 s + push (0,2–1 s, molto di più su Android in Doze senza `priority: high`) +
  download dell'immagine (0,2–1 s), cioè **~4–5 s**;
- se il companion non riesce ad aprire lo stream, **la notifica non parte proprio** (lo dice il README).

## 3. Proposte per il ritardo della notifica

| # | Intervento | Dove | Effetto atteso | Sforzo |
|---|---|---|---|---|
| P0 | Notifica in due fasi | automazione HA | avviso a T0 + push (~0,3–1 s) invece di ~4–5 s | basso |
| P1 | Entità `event` campanello, prefetch della foto, blueprint | integrazione (+ payload eventi nel companion) | trigger per entrypoint; un round-trip HA→companion in meno sul download | basso-medio |
| P2 | Dal primo pacchetto al JPEG più in fretta | companion | 154 ms → ~60–90 ms (stima da misurare) | medio |
| P3 | Foto giusta su HomeKit | companion | Apple Home mostra il visitatore attuale | medio |
| P4 | Richiesta AV reattiva (esperimento) | companion | nessun guadagno di latenza (provato, §9.1); niente NACK né retry | basso |

### P0 — Notifica in due fasi (subito, senza release)

```yaml
automation:
  - alias: Citofono - avviso immediato e foto
    mode: restart
    triggers:
      - trigger: state
        entity_id: binary_sensor.REPLACE_ringing
        from: "off"
        to: "on"
    actions:
      # 1. Avviso immediato, senza foto
      - action: notify.mobile_app_REPLACE_PHONE
        data:
          title: Citofono
          message: Hanno suonato alla porta
          data:
            tag: citofono
            ttl: 0                      # Android: consegna immediata anche in Doze
            priority: high
            channel: Citofono
            importance: high
            clickAction: /lovelace/citofono   # Android
            url: /lovelace/citofono           # iOS
            push:
              interruption-level: time-sensitive
      # 2. Appena c'è la foto del ring, la stessa notifica viene aggiornata
      - wait_for_trigger:
          - trigger: state
            entity_id: image.REPLACE_last_doorbell_ring
            not_to:
              - unknown
              - unavailable
        timeout: "00:00:15"
        continue_on_timeout: false
      - action: notify.mobile_app_REPLACE_PHONE
        data:
          title: Citofono
          message: Hanno suonato alla porta
          data:
            tag: citofono
            image: /api/image_proxy/image.REPLACE_last_doorbell_ring
            alert_once: true            # Android: l'aggiornamento non suona di nuovo
            ttl: 0
            priority: high
            channel: Citofono
            clickAction: /lovelace/citofono
            url: /lovelace/citofono
            push:
              interruption-level: passive   # iOS: sostituzione silenziosa
```

- **Android:** `ttl: 0` e `priority: high` evitano i ritardi di Doze, che da soli possono valere più di tutto
  il resto. Con `alert_once: true` l'aggiornamento con la foto non risuona.
- **iOS:** `time-sensitive` per l'avviso. Con lo stesso `tag` l'aggiornamento sostituisce la notifica e
  `passive` lo rende silenzioso. Il comportamento della sostituzione va verificato sul proprio iPhone.
- **Tocco sulla notifica:** apre la dashboard con la card, che si aggancia allo stream del ring holder già
  aperto e riceve subito il keyframe in cache. Nel log join e primo RTP sono allo stesso millisecondo
  (09:46:25.521) e l'ICE si connette 151 ms dopo.
- **Robustezza:** se lo stream non parte, l'avviso arriva comunque. Oggi invece non arriva niente.
- Con più entrypoint il `binary_sensor` è unico per dispositivo (attributo `entrypoint_id`): P1.1 lo risolve.

### P1 — Integrazione

1. **Entità `event` per entrypoint** (`EventDeviceClass.DOORBELL`, evento `ring`), emessa quando compare
   `physical_ring` per quell'entrypoint. Il `binary_sensor` attuale è unico per dispositivo e resta `on` per
   tutto lo squillo, quindi una seconda pressione durante il ring non lo riattiva. L'entità `event` è il tipo
   standard di HA per i campanelli. Ancora meglio se il companion arricchisce il payload di `BroadcastEvent`
   con `entrypoint_id`, l'id del ring o del dialogo e un timestamp. Oggi il payload contiene solo `type`
   ([run.go:608](../../internal/app/run.go#L608)) e l'integrazione lo usa solo nella diagnostica
   (`coordinator.py:84-85`).
2. **Prefetch della foto.** Quando cambia `ring_snapshot_updated_at`, `CompanionRingSnapshotImage` scarica
   subito il JPEG in memoria (in `_handle_coordinator_update`) e `/api/image_proxy` risponde dalla cache. Così
   il round-trip HA→companion esce dal percorso del telefono e N telefoni costano un solo fetch.
3. **Blueprint** con l'automazione P0, e aggiornamento della sezione README *Doorbell notifications with the
   picture*, che oggi consiglia il trigger sull'entità `image`.

### P2 — Companion: dal primo pacchetto al JPEG

I 154 ms si dividono in 32 ms per ricevere l'IDR (~60 KB al ritmo dell'intercom, non comprimibili) e 121 ms
dentro `gst-launch … h264parse ! imxvpudec ! jpegenc quality=90 ! fdsink`
([snapshot.go:562](../../internal/media/snapshot.go#L562)). Il processo parte già all'arm, ~3 s prima, quindi
i 121 ms sono init del VPU, decodifica ed encoding JPEG software.

1. **Encoder JPEG del VPU.** Il VPU dell'i.MX6 codifica anche MJPEG. Se sul device esiste `imxvpuenc_mjpeg`
   (`gst-inspect-1.0 imxvpuenc_mjpeg`), con `imxvpudec ! imxvpuenc_mjpeg ! fdsink` spariscono la copia e
   l'encoding software, che è probabilmente la voce più grossa dei 121 ms.
2. **Da `quality=90` a `80`.** Il JPEG si riduce (stima −30/40 % su 62 KB) e si codifica prima. Aiuta anche
   il download sul telefono in rete mobile.
3. **EOS dopo il primo IDR.** Chiudere stdin dopo il primo access unit forza il drain del decoder e rende
   l'uscita deterministica. Nel ring di questo log non serve (`fed_access_units=1`), ma nella live view
   successiva `fed_access_units=3` e `decode=175ms`: il VPU ha trattenuto il frame per due access unit. Il
   fallback attuale, che riavvia il decoder sul keyframe successivo (`snapshotMaxDecoders`), resta valido.
4. **Decoder pre-riscaldato.** Solo se le misure dicono che domina l'init del VPU: all'arm gli si dà l'IDR del
   ring precedente e se ne scarta l'uscita.

Per misurare sul device: estrarre l'IDR dalla clip del ring sul PC
(`ffmpeg -i ring.mp4 -frames:v 1 -c copy -bsf:v h264_mp4toannexb idr.h264`), copiarlo sull'intercom e
cronometrare le varianti:

```sh
time gst-launch-1.0 -q fdsrc fd=0 ! h264parse ! imxvpudec ! fakesink < idr.h264
time gst-launch-1.0 -q fdsrc fd=0 ! h264parse ! imxvpudec ! jpegenc quality=90 ! fakesink < idr.h264
time gst-launch-1.0 -q fdsrc fd=0 ! h264parse ! imxvpudec ! imxvpuenc_mjpeg ! fakesink < idr.h264
```

Le differenze tra le righe isolano il costo dell'encoder. In esercizio il log `snapshot captured` riporta già
`since_first_packet` e `decode`.

### P3 — HomeKit allega la foto del ring precedente (bug)

- `Manager.Sync` fa suonare il campanello HomeKit appena compare `PhysicalRing`, cioè a T0
  ([lifecycle.go:177](../../internal/homekit/lifecycle.go#L177)).
- Ricevuto l'evento campanello, Apple Home chiede uno snapshot per la notifica (`/resource`) e il companion
  risponde con `SnapshotProvider.Latest` ([lifecycle.go:540](../../internal/homekit/lifecycle.go#L540)).
  A T0 quel file è ancora quello del ring precedente o dell'ultima live view: la foto giusta esiste solo a
  T0+3,3 s.
- Proposta: aggiungere a `SnapshotManager` un'attesa del prossimo `publish`
  (`WaitRing(ctx, entrypointID, after time.Time)`, segnalata dentro `publish`). L'handler `/resource`
  registra in `Sync` l'istante di inizio del ring; se c'è un ring in corso e la foto ring è più vecchia,
  attende entro un limite da tarare sul timeout di Apple Home (es. 5 s) e risponde con quella nuova. La stessa
  primitiva permette un `?wait=` opzionale su `snapshot/ring.jpg`, per chi preferisce un'unica notifica con la
  foto giusta.

### P4 — Richiesta AV reattiva (esperimento)

Oggi il ringing preview chiede il video appena arriva l'INVITE (+0,17 s), mentre l'intercom sta ancora
attivando la telecamera. Nella variante reattiva non chiede niente finché l'intercom non avvia i propri stream
(`*7*300#127#0#0#1#5002#1` o `#5007#0`, che il frame observer vede già in
[run.go:153](../../internal/app/run.go#L153)), e solo allora manda la richiesta. A quel punto l'ACK arriva in
pochi millisecondi, come nella live view on-demand di oggi (4,3 ms). È quello che fa c300x-controller sullo
stesso hardware: al ring non chiede niente, e aggiunge il suo stream quando vede `*7*300#127#0#0#1#5007#0*##`
(`lib/handlers/openwebnet-handler.js:51-78`).

- **Fallback:** se il frame non arriva entro ~3 s dall'INVITE, la richiesta parte come oggi, con i retry.
- **Keyframe iniziale:** non si perde. Tra l'avvio degli stream dell'intercom e il primo RTP passano almeno
  0,48 s in tutti gli 11 avvii con i frame registrati. Nella live view di oggi la nostra destinazione è stata
  aggiunta ~20 ms dopo quella del display e ha ricevuto comunque l'IDR iniziale
  (`nal_types_until_keyframe=9+7+8+5`, `skipped_access_units=0`).
- **Lettura dell'esito:** se il rilancio `*7*58` sparisce e gli stream dell'intercom partono prima, era la nostra
  richiesta anticipata a provocarlo, e si guadagna fino a ~1 s su snapshot, clip e preview. Se la sequenza
  resta uguale, il NACK è innocuo e il tema si chiude con un dato certo. La modalità reattiva resta comunque più
  pulita (nessuna connessione trattenuta per 1,4 s, nessun retry): costa al massimo ~50 ms di ACK più tardo,
  che non spostano il primo RTP.
- **Misura:** una decina di ring per modalità, dietro un flag di configurazione, confrontando gli istanti di
  `*7*58` ripetuto, stream dell'intercom e primo RTP. Una riga di log riassuntiva per ring rende il confronto
  immediato.

## 4. Opzioni valutate e scartate

- **Avviare lo stream dal frame OpenWebNet invece che dall'INVITE:** anticipa la richiesta di 99 ms ma non
  l'ACK (§1.1). In più il preview prende in prestito la chiamata SIP, che esiste solo dopo l'INVITE.
- **Retry più rapido:** l'ACK arriva insieme agli stream dell'intercom (§1.2), quindi un retry anticipato lo
  riceverebbe nello stesso istante.
- **Richiesta solo dopo `*7*77`:** provata in beta.10 senza guadagno, secondo l'analisi precedente (log non
  ritrovati). `*7*77` cade comunque dentro la finestra di attivazione, quindi non esclude l'ipotesi di P4.
- **Intercettare lo stream del display (:5002):** parte nello stesso istante del nostro.
- **Early media SIP (183 con SDP sull'INVITE in ingresso):** il video passerebbe dalla stessa pipeline
  dell'intercom, pronta allo stesso istante. Aggiungerebbe SRTP e il rischio di disturbare il forking della
  chiamata verso gli altri dispositivi. ha-bticino-c300x usa proprio questa strada per il preview (§8), ma
  non ne misura la latenza. Nel nostro log gli stream dell'intercom (`#5002#1` e `#5007#0`, alle 18.414–18.416)
  partono 126 ms dopo il nostro ACK (18.288). Lo stack SIP dell'intercom attinge probabilmente da uno di
  questi, quindi l'early media non arriverebbe prima.
- **Decodificare l'IDR in HA invece che sul device:** risparmia ~0,1 s ma porta H.264 grezzo in HA e una
  dipendenza da PyAV. P2 ottiene quasi lo stesso risultato dentro il companion.

## 5. Altri spunti di performance

| # | Osservazione (evidenza) | Proposta | Guadagno | Sforzo |
|---|---|---|---|---|
| 1 | `AVClient.waitForFlow` controlla il flusso ogni 100 ms ([av.go:185](../../internal/openwebnet/av.go#L185)). Nel log costa 85 ms (video) + 98 ms (audio) nella live view e 39 ms nel ring | Il ricevitore segnala il primo pacchetto con un canale, al posto del ticker | ~180 ms per ogni avvio on-demand | basso |
| 2 | `Offer` chiama `AcquireRingingPreview` prima di `CreateAnswer` ([webrtc.go:371](../../internal/media/webrtc.go#L371)–[396](../../internal/media/webrtc.go#L396)): ICE e DTLS (49.686 → 49.946, 260 ms) partono solo dopo 4,4 s di avvio dell'intercom | Negoziare subito e collegare le tracce quando il lease è pronto | ~250 ms on-demand | medio |
| 3 | `bridgeSource.Start` avvia i 4 `gst-launch` prima della sorgente ([run.go:725](../../internal/app/run.go#L725)): 87 ms on-demand prima dell'INVITE, 60 ms nel ring. Nel ring sono 4 processi a ogni squillo anche se nessuno risponde | Avviarlo in parallelo alla sorgente. Nel preview valutare l'avvio alla risposta, in parallelo alla richiesta audio | −87 ms on-demand, meno CPU al ring | basso |
| 4 | `newEventApplier` scrive stato ed evento a ogni client WebSocket nel goroutine del chiamante (listener OpenWebNet, SIP), con deadline di 5 s per client ([websocket.go:154](../../internal/api/websocket.go#L154), [:197](../../internal/api/websocket.go#L197)). Un HA lento o una rete degradata bloccano la lettura dei frame, `ring.started` compreso | Coda per client non bloccante, con coalescenza degli `state` (conta solo l'ultimo) | evita ritardi rari ma lunghi sul ring | medio |
| 5 | Ogni frame `*7*` arriva 2–4 volte (es. `*7*300#127#0#0#1#46168#1*##` alle 18.297, 18.321 e 18.323) e ogni copia passa da `ObserveControlTrack` e dal log | Dedup nel listener dei frame idempotenti ripetuti entro ~100 ms | piccolo | basso |
| 6 | Lo snapshot scade 8 s dopo l'arm ([snapshot.go:29](../../internal/media/snapshot.go#L29)), mentre i retry AV durano fino a 6 s più 1,2 s di attesa del flusso ([av.go:49](../../internal/openwebnet/av.go#L49)). Con un intercom lento lo stream parte ma la foto no, e oggi neanche la notifica | Far partire il timeout dal primo pacchetto o allinearlo alla finestra AV | robustezza | basso |
| 7 | A ogni ring finiscono su flash due JPEG (latest e ring, sotto lock e prima del broadcast) e la clip (1,4 MB). A livello DEBUG si aggiungono ~150 righe di log in 3 s | **Fatto in beta.6** (§9.4): foto in memoria, servita da lì e salvata in modo asincrono. Resta il log a INFO in esercizio | ~40 ms sul ring (la partizione dati è montata `sync`), meno usura della flash | basso-medio |
| 8 | ICE `disconnected` alle 09:46:58.694, ma la sessione resta attiva fino al BYE dell'intercom alle 09:47:20.475 (22 s) con AV, pipeline audio e ricevitori accesi | Dopo `disconnected`, un breve periodo di grazia e poi il rilascio del lease | libera l'intercom prima | basso |
| 9 | Il contatore della connessione TCP verso Flexisip passa da `ref=2` (REGISTER prima del ring) a `ref=4` (REGISTER delle 09:54) dopo una chiamata in ingresso e una in uscita: una transazione INVITE non rilascia il suo riferimento (sipgo) | Verificare se cresce a ogni chiamata. È innocuo finché la connessione resta persistente | da verificare | da valutare |
| 10 | Quando il ring finisce normalmente, la sessione WebRTC ancora aperta viene chiusa con `reason="source failure"` (09:46:33.085) | Usare un motivo `ring ended`: rende chiari il log e la card | solo diagnostica/UX | basso |

## 6. Stream: altri spunti

Il video segue un unico percorso: socket RTP → `RTPReceiver.read` → `snapshotAttempt.Consume` →
`StreamCoordinator.ObserveRTPPacket` → `fanout.video` → ogni consumatore (WebRTC, HomeKit, RTSP, clip). Tutto
gira nella goroutine del ricevitore, a ~99 pacchetti/s da ~1 KB (log: 2 557 pacchetti in 25,8 s, 816 281 byte
in 791 pacchetti). L'audio passa per l'audio bridge: due processi GStreamer e tre salti UDP per direzione.

| # | Tema | Guadagno | Sforzo |
|---|---|---|---|
| S1 | Audio bridge: i pacchetti in coda condividono il buffer di lettura (bug latente) | audio senza frame ripetuti o corrotti sotto carico | molto basso |
| S2 | Audio bridge da 4 processi a 2, senza ricampionamenti (Opus a 8 kHz, oppure PCMU verso il browser) | metà dei processi, meno CPU per chiamata, un salto in meno | medio |
| S3 | Lo snapshot clona ogni pacchetto per tutta la durata dello stream | ~100 KB/s di garbage e ~280 KB trattenuti in meno | molto basso |
| S4 | Fan-out sincrono nella goroutine del ricevitore (`SetReadBuffer` fatto in beta.6, §9.4) | isolamento tra consumatori | medio |
| S5 | Mutex del coordinator preso a ogni pacchetto | percorso dei pacchetti libero dal controllo | basso-medio |
| S6 | Allocazioni per pacchetto (HomeKit) e misura del GC | meno lavoro del GC su ARM | basso |
| S7 | Card e RTSP/HomeKit si escludono a vicenda | niente riavvii da ~4,4 s al cambio di consumatore | medio |
| S8 | Live view e chiamata risposta chiuse dall'intercom dopo ~31 s: keepalive in chiaro su una chiamata SRTP. **Fatto in beta.8** (§9.4) | da 31 a 60 s | basso |
| S9 | Live view chiusa dall'intercom dopo 60 s anche con il keepalive SRTP | visioni oltre il minuto | basso |

**S1 — Bug latente: l'audio bridge riusa il buffer dei pacchetti in coda.** Il lettore legge ogni datagramma
nello stesso buffer da 1 500 byte e mette il pacchetto in un canale da 32 posti
([audiobridge.go:549](../../internal/media/audiobridge.go#L549)). `rtp.Packet.Unmarshal` non copia il
payload: punta al buffer. Se la goroutine di inoltro resta indietro di più di un pacchetto (20 ms), i payload in
coda vengono sovrascritti dal datagramma successivo. Succede nei momenti di carico (GC, avvio di processi, SRTP)
e produce frame ripetuti o corrotti, sia nell'Opus verso le card sia nello Speex verso l'intercom. Nel
ricevitore video lo stesso schema è sicuro, perché il pacchetto viene consumato prima della lettura successiva e
chi lo conserva lo clona.
*Proposta:* un buffer nuovo per datagramma, o `bytes.Clone` del payload prima di accodarlo, più un test con
`-race`.

**S2 — Audio bridge: 4 processi e due ricampionamenti.** Ogni direzione passa per due `gst-launch` e tre salti
UDP su loopback: lo Speex lo gestisce il GStreamer 1.12 di sistema, l'Opus quello del bundle
([audiobridge.go:385](../../internal/media/audiobridge.go#L385)). In mezzo c'è L16 a 8 kHz, poi un
`audioresample` a 48 kHz prima di `opusenc` (24 kbit/s) e uno da 48 a 8 kHz dopo `opusdec`.
*Proposte:*

- aggiungere il plugin `speex` al GStreamer del bundle e usare un solo processo per direzione
  (`udpsrc ! rtpspeexdepay ! speexdec ! audioconvert ! opusenc … ! rtpopuspay ! udpsink` e il simmetrico);
- togliere i ricampionamenti: `opusenc` accetta 8 kHz in ingresso e `opusdec` può uscire a 8 kHz. La sorgente
  è narrowband, quindi bastano `bandwidth=narrowband` e 12–16 kbit/s;
- facoltativo: tenere vive le pipeline tra una sessione e l'altra, così ring e live view non pagano l'avvio dei
  processi (in cambio di RAM occupata sempre);
- facoltativo, per la qualità: nell'uplink `rtpjitterbuffer` prima di `rtpopusdepay` e `opusdec plc=true`, per
  mascherare i pacchetti persi in Wi-Fi.

*Alternativa: PCMU verso il browser* (è la scelta di ha-bticino-c300x, §8). Restano i due processi Speex del
GStreamer di sistema, cioè gli stadi `downlink-speex-decode` e `uplink-speex-encode` di oggi. La conversione
L16 ↔ µ-law la fa il companion con una tabella, e la sessione WebRTC negozia PCMU invece di Opus: tutti i browser
lo supportano e pion lo registra come `MimeTypePCMU`. Spariscono così i due processi del GStreamer del bundle,
l'encoder Opus e i ricampionamenti, senza toccare il bundle. HomeKit però non accetta PCMU, quindi lì resta Opus.

*Esperimento a parte, più invasivo:* ha-bticino-c300x ha un'opzione sperimentale che porta in PCMU l'intercom
stesso. Modifica `<enable_speex>` in `/home/bticino/cfg/stack_open.xml` e il codec in `/etc/linphone.conf`, con
backup e riavvio. Se il C100X la supporta e lo stream AV audio la segue, l'audio WebRTC passerebbe senza nessuna
conversione. Va verificato sul device: oggi l'INVITE del C100X offre solo `speex/8000` e G.729 (PT 18).

*Guadagno:* metà dei processi (RAM e avvio, oggi 60–87 ms), meno CPU per chiamata, un salto in meno per
direzione.

**S3 — Lo snapshot clona ogni pacchetto per tutta la durata dello stream.** `Consume` clona il pacchetto dentro
il `select`, e Go valuta l'espressione da inviare prima di sapere se il canale ha posto
([snapshot.go:211](../../internal/media/snapshot.go#L211)). Dopo la foto, o dopo il timeout, nessuno legge più
il canale: ogni pacchetto video viene comunque clonato e scartato (~100 KB/s di garbage), e i 256 cloni in coda
(~280 KB) restano in memoria finché lo stream non si chiude.
*Proposta:* un flag atomico impostato alla fine della cattura e controllato prima del `Clone`; svuotare il
canale.

**S4 — Fan-out sincrono.** `fanout.video` serve tutti i consumatori in linea, nella goroutine del ricevitore e
sotto `deliveryMu` ([fanout.go:161](../../internal/media/fanout.go#L161)). Per ogni pacchetto passano di lì
interceptor e SRTP di ogni sessione WebRTC, marshal e SRTP di HomeKit, RTSP e clip. Oggi le scritture UDP non
bloccano e il costo è solo CPU. Un consumatore che si blocca, però (TURN su TCP verso un telefono in rete
mobile, per esempio), ferma tutti gli altri e la lettura del socket. Il socket RTP usa il buffer di ricezione
di default ([rtp_receiver.go:88](../../internal/media/rtp_receiver.go#L88)), mentre un IDR arriva come ~45
pacchetti in ~30 ms.
*Proposta:* una coda limitata e una goroutine per consumatore; se la coda si riempie, scartare fino al
keyframe successivo. `SetReadBuffer` (~1 MB) sui socket RTP. Su un SoC multi-core le sessioni cifrano anche in
parallelo. ha-bticino-c300x, sullo stesso tipo di hardware, imposta 512 KiB sul socket video e drena fino a 64
pacchetti per risveglio (§8). In entrambi i casi il kernel limita il valore a `net.core.rmem_max`, da leggere
sul device.

**S5 — Mutex del coordinator a ogni pacchetto.** `ObserveRTPPacket` prende il mutex principale del
coordinator per ogni pacchetto video e audio (~150/s) e ricalcola `streamHealth` con `time.Now()`
([stream_coordinator.go:1123](../../internal/media/stream_coordinator.go#L1123)). È lo stesso mutex di
acquire, join, leave e stop.
*Proposta:* contatori e istanti in variabili atomiche; lo stato di salute calcolato quando serve (`Snapshot`, e
`watch` ogni secondo).

**S6 — Allocazioni per pacchetto e GC.** Per ogni pacchetto video si allocano la struttura del ricevitore, il
clone dello snapshot (S3), il clone per la cache del keyframe (necessario) e quello per la clip (necessario, 15
s). HomeKit aggiunge clone del payload, `Marshal` ed `EncryptRTP(nil, …)`: tre allocazioni e due copie
([camera.go:514](../../internal/homekit/camera.go#L514)).
*Proposta:* buffer riusati per sessione HomeKit (`MarshalTo`, `EncryptRTP` con destinazione riusata).
Misurare il GC sul device con `GODEBUG=gctrace=1` durante una visione con due card; solo dopo, valutare
`GOGC`/`GOMEMLIMIT`.

**S7 — Card e RTSP/HomeKit si escludono a vicenda.** RTSP usa `Acquire` e HomeKit `AcquireWithStartup`, che non
sono condivisibili: davanti allo stream di una card ricevono `ErrStreamBusy`, e una card non può agganciarsi a
uno stream aperto da loro ([stream_coordinator.go:446](../../internal/media/stream_coordinator.go#L446)).
Quando il primo chiude, il secondo riparte da zero: ~4,4 s on-demand.
*Proposta:* rendere condivisibili anche RTSP e HomeKit (stesso fan-out, replay del keyframe), dopo aver
verificato le regole su risposta e backchannel.

**S8 — Live view chiusa dall'intercom dopo ~31,6 s.** La live view on-demand delle 09:46:45 è stata chiusa
dall'intercom con un BYE 31,6 s dopo il 200 OK (09:46:48.870 → 09:47:20.472), con il keepalive RTP attivo: un
pacchetto ogni 5 s sulla porta audio ([keepalive.go](../../internal/signaling/keepalive.go)). Ogni visione
oltre ~30 s si interrompe, e ripartire costa ~4,4 s. In questo log non ha pesato, perché la card era già chiusa
dalle 09:46:58.
*Causa più probabile:* la nostra offerta SDP dichiara SRTP su audio e video (`RTP/SAVP` con
`a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:dummykey`, [sdp.go:153](../../internal/signaling/sdp.go#L153)), ma il
keepalive parte in chiaro. Lo stack SIP dell'intercom è linphone (`belle-sip/1.6.3` nel 200 OK). Se ha accettato
SRTP, scarta il pacchetto perché non supera l'autenticazione e non lo conta come RTP ricevuto. linphone chiude
la chiamata dopo `nortp_timeout` secondi senza RTP, 30 di default, e i conti tornano con i 31,6 s.
ha-bticino-c300x tiene in vita lo stesso tipo di chiamata con chiavi SDES vere e pacchetti SRTP: un byte di
silenzio sull'audio ogni 20 ms, RTCP ogni secondo e STUN sulle 4 porte ogni 500 ms (§8).
*Verifica:* loggare l'SDP del 200 OK, per vedere se l'intercom risponde con `a=crypto`, e leggere
`nortp_timeout` nella sezione `[rtp]` di `/etc/linphone.conf` sul C100X.
*Proposta:* una chiave SDES vera per dialogo (16 byte di chiave e 14 di salt, in base64) al posto di
`dummykey`, e il keepalive cifrato con `srtp.CreateContext` e il profilo `ProtectionProfileAes128CmHmacSha1_80`.
HomeKit fa già lo stesso ([camera.go:634](../../internal/homekit/camera.go#L634)). Il payload resta il byte
`0x00`, lo stesso che ha-bticino-c300x usa come silenzio Speex. Se invece l'intercom risponde senza SRTP, la
causa è un'altra: allora keepalive ogni secondo e anche sulla porta video, poi un re-INVITE prima dei 30 s. Se
dopo il fix il video si ferma comunque a intervalli fissi, c'è un precedente: ha-bticino-c300x ripete la
richiesta `*7*300` ogni 20 s.
*Esito:* l'intercom accetta SRTP (§9.4) e la proposta è in beta.8. La live view passa da 31 a 60 s.

**S9 — Live view chiusa dall'intercom dopo 60 s.** Con il keepalive SRTP la live view delle 18:00:56 è durata
60,4 s: BYE SIP dell'intercom alle 18:01:56.743, nello stesso millisecondo di `*7*55`, `*7*219` e `*7*73#0#0`,
cioè della chiusura della sessione AV. Video e audio sono arrivati regolari fino alla fine. In
`/etc/linphone.conf` non c'è nessun limite a 60 s.
*Ipotesi:* la sessione AV scade se non viene rinnovata. ha-bticino-c300x, durante la visione, ripete la
richiesta `*7*300` ogni 20 s (`MEDIA_RENEW_SECONDS` in `media_bridge.c:58`) e manda un receiver report SRTCP
ogni secondo sulle porte RTCP di audio e video (`media_bridge.c:4002`).
*Proposta:* rinnovare la richiesta AV video ogni 20 s finché lo stream è attivo, per la live view e per la
chiamata risposta; se non basta, aggiungere gli SRTCP RR. **Non prioritario:** 60 s bastano per l'uso
attuale.

## 7. Ordine suggerito

Stato al 31/10: P4, S1, S3 e P2 (punti 1–3) sono in v0.12.1-beta.5; §5 punto 7, `SetReadBuffer` (S4) e S8
in v0.12.1-beta.8, con gli esiti del §9. P0 è applicato in Home Assistant (automazione
`BTicino_Doorbell_Pressed`). Restano aperti P1, P3, P2.4, S9 e il resto.

1. **P0 subito**, poi misurare il tempo tra ring e notifica sul telefono.
2. **P4** (esperimento sulla richiesta reattiva), **S1** e **S3**: poche righe e rischio basso. P4 è l'unica
   strada nota per accorciare lo snapshot di più di qualche decina di millisecondi.
3. **S8** (keepalive SRTP): fatto in beta.8. **S9** (limite dei 60 s) solo se servono visioni più lunghe.
4. **P3** (bug) e **P1.2** (prefetch): interventi piccoli e indipendenti.
5. **Misure sul device**: P2 (snapshot), GC (S6), `net.core.rmem_max` (S4), e se l'intercom può passare a PCMU
   (S2).
6. **S2** (audio bridge: PCMU verso il browser oppure Opus a 8 kHz), poi **§5** punto 4 (broadcast) e punti 1–3
   (live view).
7. **S4, S5, S7**, quando si rimette mano al fan-out e al coordinator.

## 8. Confronto con ha-bticino-c300x

[ha-bticino-c300x](https://github.com/memphi2/ha-bticino-c300x) (v1.9.4, checkout locale @ `e3dd8dd`) è
un'integrazione HA in Python con un agent nativo in C sull'intercom. Il supporto al C100X è sperimentale e copre
eventi, apriporta, luce scale, mute e live view on-demand in bassa risoluzione. Ring Call e preview sul C100X sono
una fase futura, e il loro piano rimanda ai dati di BTicinoGO (`docs/superpowers/plans/2026-09-24-c100x-support.md`).

| Tema | ha-bticino-c300x | Companion | Cosa ne ricaviamo |
|---|---|---|---|
| Video del ring | Early media SIP. Si registra nel gruppo `alluser` e risponde 100 e 180, poi aspetta 300 ms. Manda 183 con SDP (video `recvonly`, audio `inactive`) e decifra l'SRTP con la `libsrtp` del firmware (`media_bridge.c:2874`, 183 a `:2964`). Durante il ring non manda nessuna richiesta `*7*300` | Richiesta `*7*300` all'INVITE, NACK, retry | Nessun guadagno atteso (§4). Senza `*7*300` non hanno il NACK, ma non misurano la latenza: per P4 non ci sono dati nuovi |
| Snapshot del ring | Nessuno sul device. Ring Capture registra in HA con `ffmpeg` dallo stream RTSP e poi estrae i JPEG (`ring_capture.py`) | IDR decodificato sul device, 0,16 s dopo il primo pacchetto | Il nostro percorso è più rapido |
| Notifica | Blueprint su `doorbell_pressed`, inviata subito e senza immagine. Android: `tag`, canale, `priority: high`, `ttl: 0`, `importance: max`, azioni. iOS: `interruption-level: critical` e `category: camera` con l'`entity_id` della camera, che mostra la vista dal vivo quando si espande la notifica (`doorbell_call_ios.yaml:172–181`) | P0 da adottare | Conferma P0. Da noi la vista dal vivo su iOS non mostrerebbe nulla: la `camera` è solo WebRTC, senza HLS né immagini fisse, e servirebbe almeno `async_camera_image`. `critical` è un'alternativa a `time-sensitive` per chi vuole il suono anche in silenzioso |
| Live view on-demand | INVITE che ricalca l'SDP dell'app ufficiale, con chiavi SDES vere, poi `*7*300` due volte a 800 ms di distanza (`media_bt_av.c:133`). Durante la visione: silenzio SRTP ogni 20 ms, RTCP ogni secondo, STUN ogni 500 ms e `*7*300` ripetuto ogni 20 s (`media_bridge.c:3845`) | `dummykey`, keepalive in chiaro ogni 5 s, BYE a 31,6 s | S8 |
| Audio | Tutto in PCMU dentro l'agent: lo Speex lo decodifica la `libspeex` del firmware, caricata con `dlopen`, e go2rtc porta il PCMU al browser. In più un'opzione sperimentale che porta in PCMU l'intercom stesso (`audio_codec.c`) | 4 `gst-launch`, Opus a 48 kHz | S2, alternativa PCMU |
| Ricezione RTP | `SO_RCVBUF` a 512 KiB sul socket video, fino a 64 pacchetti drenati per risveglio (`media_bridge.c:56–57`, `:5439`) | Buffer di default | S4 |

Da lì non viene altro di utile. Il PLI che mandano ogni secondo sul video SRTP non ha un effetto documentato, e
sullo stream AV in chiaro che usiamo noi non c'è un canale RTCP su cui mandarlo.

## 9. Prove del 31/10

Ring sul C100X con le beta v0.12.1-beta.2 → beta.5, log DEBUG letti via SSH. Tempi in secondi da
`ring.started` (T0). Dalla beta.3 ogni ring scrive una riga `ring video timeline` (component `ring_video`)
con gli stessi istanti in millisecondi.

### 9.1 P4 — richiesta AV reattiva

**Com'è fatta.** Il ringing preview non chiede il video all'INVITE: aspetta il primo `*7*300#127#0#0#1#5002#…`
o `#5007#…` dell'intercom e solo allora manda la propria richiesta. Se il frame non arriva entro 3 s, la
richiesta parte comunque, con i retry di sempre (`internal/app/ringwatch.go`). La beta.1 la metteva dietro una
chiave di configurazione, tolta nella beta.2: il confronto con la modalità precedente lo danno già i ring del
§1.2.

**Bug trovato nella beta.2.** Il frame del ring `*8*1#1#4#21*11##` arriva al frame observer tre volte (a +0,
+318 e +330 ms). Ogni copia faceva ripartire il ring e sostituiva il segnale atteso dalla richiesta, che così
partiva sempre col fallback di 3 s, ~0,7 s dopo lo stream dell'intercom: si perdeva l'IDR iniziale (6 access
unit scartati) e la foto arrivava a +3,86 s. Corretto nella beta.3: le copie entro 1 s vengono ignorate, e un
ring nuovo sostituisce il segnale solo dopo che lo stream del precedente è arrivato.

| Ring | `*7*58` ripetuto | stream dell'intercom | nostra richiesta | ACK | primo RTP | foto |
|---|---:|---:|---:|---:|---:|---:|
| 15:59:48 (beta.2, bug) | +1,59 | +2,47 | +3,18 (fallback) | 4 ms | +3,22 | +3,86 |
| 16:34:10 | +1,52 | +2,40 | +2,41 | 13 ms | +2,94 | +3,13 |
| 16:59:30 | +1,60 | +2,52 | +2,52 | 3 ms | +3,07 | +3,22 |
| 17:00:02 (b) | +1,10 | +1,99 | +1,99 | 3 ms | +2,54 | +2,67 |
| 17:00:37 (b) | +1,24 | +2,13 | +2,13 | 5 ms | +2,82 | +3,02 |

(b) Squilli arrivati 33–35 s dopo il ring precedente: di nuovo i più rapidi, come nel §1.2.

Cosa ne segue:

1. **L'ipotesi del §1.2, punto 5, è smentita.** Nel ring delle 15:59 il companion non ha chiesto niente fino a
   +3,18 s, eppure l'intercom ha rilanciato l'attivazione (`*7*58`) a +1,59 s e ha avviato i suoi stream a
   +2,47 s, come quando la richiesta parte a +0,17 s. Il rilancio è dell'intercom, e lo fa a ogni ring.
2. **Nessun guadagno di latenza.** Primo RTP e foto restano negli intervalli di prima (§1.2: primo RTP
   +2,33–3,25 s).
3. **La modalità reattiva resta** perché è più pulita: un solo tentativo, ACK in 3–13 ms, nessuna connessione
   trattenuta 1,4 s, nessun retry. L'IDR iniziale non si perde: arriva 16–40 ms dopo il primo pacchetto, con 0
   access unit scartati.
4. **Il confronto con c300x-controller** (§3, P4) regge solo nel principio. Loro reagiscono a `#5007#0` soltanto,
   chiedono anche l'audio 300 ms dopo, ritentano il NACK ogni secondo fino a tre volte e non hanno un fallback.

### 9.2 P2 — dal primo pacchetto al JPEG

**Misure sul device**, con la clip del ring delle 15:59 (161 frame, 640×480, 12 fps) convertita in Annex-B sul
PC. Il GStreamer di sistema non ha `qtdemux`, quindi le clip MP4 non si leggono sul citofono.

| Pipeline dopo `imxvpudec` | per frame (clip intera) | JPEG dell'IDR |
|---|---:|---:|
| `fakesink` (solo decodifica) | ~3 ms | |
| `jpegenc quality=90` (prima) | ~29 ms | 57,7 KB, uguale byte per byte alla foto del ring |
| `jpegenc quality=80` | ~28 ms | 40,4 KB |
| `imxvpuenc_mjpeg` (qualità di default) | ~2 ms | 40,1 KB, visivamente identico |

Con il decoder avviato 2 s prima, come in produzione, dall'IDR scritto al JPEG letto passano 85 ms con
`jpegenc quality=90` e 65 ms con `imxvpuenc_mjpeg` (3 prove ciascuno, sempre uguali). Un solo IDR seguito dalla
fine dell'input (EOS) produce sempre il JPEG.

**In produzione il VPU va peggio.** Con `imxvpuenc_mjpeg` (beta.3 e beta.4) il `decode` dei quattro ring del
§9.1 è stato di 164, 124, 110 e 156 ms (media ~140), contro i 110–121 ms di `jpegenc` nei ring precedenti. La
spiegazione più probabile è la contesa per il VPU: il nostro decoder riceve l'IDR nello stesso istante in cui
l'intercom apre il proprio flusso video, e l'encoder sul VPU si aggiunge a quella contesa. Nella beta.5 il JPEG
torna su `jpegenc`, a `quality=80`: lavora sulla CPU e tiene il file a ~40 KB.

**L'EOS dopo l'IDR non accorcia i tempi in produzione.** In tutti e quattro i ring l'immagine è uscita dopo
l'arrivo del frame successivo (`fed_access_units=2`). È rimasto perché non fa danni.

**Attenzione alle prove sotto carico.** Un ciclo di decodifiche in parallelo, lanciato per simulare la contesa,
ha esaurito la memoria fisica del driver (`mxc_vpu ... Physical memory allocation error!` in `dmesg`), e per
qualche secondo ogni decodifica falliva subito, con entrambi gli encoder. Si è ripreso da solo appena fermato il
ciclo. Prove di questo tipo non vanno ripetute sul citofono in uso.

Da verificare con la beta.5: `decode` di nuovo sui ~110–120 ms con JPEG da ~40 KB. I ~110 ms che restano sono
inizializzazione e contesa del VPU, non decodifica (~3 ms a frame). Un decoder pre-riscaldato (P2.4) eviterebbe
l'inizializzazione, ma non la contesa.

### 9.3 Stream e verifiche sul device

- **S1** (beta.3): ogni datagramma dell'audio bridge viene letto in un buffer proprio. Non ancora verificato
  con una chiamata risposta.
- **S3** (beta.3): finita la foto, lo snapshot non clona più i pacchetti e libera quelli in coda.
- **S8, una delle due verifiche:** `/etc/linphone.conf` ha `media_encryption=1` (SRTP) e nessuna sezione
  `[rtp]`, quindi `nortp_timeout` vale il default di 30 s. La seconda verifica e l'esito sono nel §9.4.
- **Correzione al §1.1:** nello stesso file la sezione `[video]` ha `rtp_io=1` e `rtp_local_port=5002`, e
  `[sound]` ha `rtp_local_port=5000`. Lo stream `#5002#1` è quindi l'ingresso video di linphone, cioè dello stack
  SIP, non del display, e `#5000#2` è l'audio verso linphone. A cosa serva `#5007#0` resta da capire.

### 9.4 Beta.6 → beta.8: snapshot in memoria, buffer RTP, S8

**§5 punto 7 (beta.6).** La partizione dati del companion (`/home/bticino/cfg/extra`) è ext4 montata
`sync,data=journal`: ogni scrittura è sincrona e passa due volte dal journal. Nel ring delle 17:00:40 le due
scritture del JPEG hanno trattenuto il broadcast per ~40 ms (mtime: `main.jpg` a .381, cartella `ring/`
a .421). Ora la foto viene annunciata e servita dalla memoria, e i file vengono scritti in background, uno alla
volta e sempre con la foto più recente. L'mtime dei file è l'istante di cattura, quindi dopo un riavvio
`ring_snapshot_updated_at` non cambia. Resta un limite: se il processo si ferma entro ~50 ms da una cattura, il
file resta quello precedente. Il guadagno sul ring è ancora da misurare.

**S4, solo il buffer (beta.6).** `net.core.rmem_max` e `rmem_default` valgono 163 840 sul C100X.
`SetReadBuffer(1 MiB)` sui socket RTP viene limitato a `rmem_max` e raddoppiato dal kernel: 320 KiB invece di
160. In 6 giorni di uptime `RcvbufErrors` segnava 2 861 datagrammi persi, senza modo di attribuirli a un
socket. Per superare `rmem_max` servirebbe `SO_RCVBUFFORCE` (il companion gira come root).

**S8 (beta.6 → beta.8).**

- **Chiamata risposta:** il problema c'era anche lì. Il 28/10 alle 16:52:08 un ring risposto dalla card è stato
  chiuso dall'intercom (`inbound sip call ended by remote`) alle 16:52:39, dopo 31,2 s, con il keepalive
  partito. L'INVITE dell'intercom offre SRTP con chiavi SDES vere
  (`a=crypto:1 AES_CM_128_HMAC_SHA1_80` e `a=crypto:2 AES_CM_128_HMAC_SHA1_32`), e noi rispondevamo con
  `dummykey`.
- **SDP del 200 OK** (live view delle 17:46, beta.6): `RTP/SAVP` con `a=crypto:1 AES_CM_128_HMAC_SHA1_80` e
  chiave vera, sia sull'audio sia sul video. L'intercom accetta SRTP. BYE di nuovo a 31,4 s.
- **Correzione (beta.8):** ogni offerta e ogni risposta portano una chiave SDES nuova per linea media, e il
  keepalive viene cifrato con la chiave audio del nostro SDP quando anche l'audio dell'altro lato dichiara SRTP
  ([keepalive.go](../../internal/signaling/keepalive.go), [sdp.go](../../internal/signaling/sdp.go)).
- **Esito:** nella live view delle 18:00:56 il log dice `sip media keepalive started … srtp=true`, e il BYE
  arriva dopo 60,4 s invece di 31. Il nuovo limite è S9. La chiamata risposta non è ancora stata riprovata.

**Release.** La beta.7 non è stata pubblicata: un test nuovo dello snapshot lasciava una scrittura in
background oltre la fine del test, e la pulizia di `t.TempDir` falliva in CI. I test ora aspettano le
scritture (`f05aad5`), e la beta.8 contiene tutto.
