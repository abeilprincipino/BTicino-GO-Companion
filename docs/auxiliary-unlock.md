# Auxiliary unlock commands

An intercom's auxiliary or staircase-light button can operate another gate in some
installations. It uses a different OpenWebNet command pair from the standard door-lock
button. Changing an entrypoint's device address alone does not select that command pair.

## Configure an entrypoint

In the Companion WebUI, open **Config > Entrypoints**. Each entrypoint has two optional
unlock settings:

| Setting | Values | Default when absent |
| --- | --- | --- |
| Unlock command (`unlock_mode`) | `door` or `auxiliary` | `door` |
| Press duration (`unlock_duration_ms`) | 1–2000 milliseconds; zero in YAML means the default | 2000 ms |

The UI calls these modes **Door lock** and **Auxiliary / staircase light**. Keep
**Unlock** enabled for an output that should have an opening button. An auxiliary-only
output normally has **Doorbell (ring + stream)** disabled. Keep the ID of an existing
entrypoint when changing its label or command settings, because Home Assistant uses the
ID to identify its entities.

Save, then choose **Admin > Restart** to apply the configuration.

## Example: standard pedestrian gate and auxiliary vehicle gate

This example is based on an installation whose known working commands are
`*8*19*20##` / `*8*20*20##` for the pedestrian gate and
`*8*21*10##` / `*8*22*10##` for the vehicle gate. These addresses are installation-specific.

```yaml
companion:
  entrypoints:
    - id: pedonale
      label: Pedonale
      devaddr: "20"
      capabilities:
        stream: true
        unlock: true
        ring: true
    - id: carrabile
      label: Carrabile
      devaddr: "10"
      unlock_mode: auxiliary
      unlock_duration_ms: 1000
      capabilities:
        stream: false
        unlock: true
        ring: false
```

This is only the entrypoint portion of the configuration; do not replace the full file
with this example. Preserve existing IDs and all pairing, network, SIP and other settings.

The pedestrian entrypoint retains its existing two-second door-lock press. The vehicle
entrypoint sends `*8*21*10##`, waits one second, then sends `*8*22*10##`. The Home Assistant
integration continues using its existing unlock API; it needs no new service or MQTT helper.

The 2000 ms limit preserves the controller's existing three-second command-session budget.
Unknown modes and invalid durations are rejected before the configuration is saved.

## Compatibility and rollback

Old configurations without these options retain their previous behavior. The WebUI omits
default values when saving. Builds predating this feature reject the new configuration
keys because their YAML parser checks for unknown fields.

Before installing a local build, back up `config.yaml` on the intercom. Follow the
[documented binary-only redeploy procedure](build-and-install.md#4-re-deploy-rapido-solo-binario)
and use a local build with an empty `BuildReleaseRepo`, so the release updater cannot
replace the local change unexpectedly.

To revert after saving auxiliary settings, restore **both** the old binary and the matching
configuration backup, while the Companion service is stopped. Restoring only the old binary
will leave it unable to load a configuration containing the new keys. Any configuration
changes made after the backup will also need to be reapplied after rollback.

No test that runs on a workstation can confirm the physical gate has opened. The software
tests verify the outgoing command pair, press interval, legacy defaults and configuration
round-trip; the final device check must use the installation's known working command sequence.
