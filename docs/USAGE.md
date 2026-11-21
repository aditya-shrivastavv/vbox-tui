# Usage guide

## Picker and batches

Run `vbx`, navigate with arrows, type to filter, and press Enter. Space toggles marks for batches. CLI names match exactly; use UUIDs for duplicates.

```powershell
vbx start "dev-linux" "sandbox"
vbx pause "dev-linux"
vbx resume "dev-linux"
vbx save "dev-linux"
vbx stop "dev-linux"
```

Batches execute sequentially and stop on an operation failure. Completed actions remain in effect. The summary distinguishes skipped and unattempted machines. Cancellation prevents later work where possible; it cannot undo an issued start or guarantee VirtualBox stopped an in-flight operation.

## Configuration

```powershell
vbx config
vbx config "dev-linux"
vbx config --path
```

The per-VM editor includes SSH settings and guest OS/restart routing, with scoped resets. The directory comes from Go's `os.UserConfigDir`, under `vbox-tui`; on Windows normally `%APPDATA%\vbox-tui`. Use the printed path instead of guessing. `--config PATH` selects another file.

Illustrative settings with a synthetic UUID and username:

```json
{
  "vms": {
    "11111111-2222-4333-8444-555555555555": {
      "name": "dev-linux",
      "ssh": { "mode": "endpoint", "host": "127.0.0.1", "port": 2222, "user": "guest" },
      "guest": { "os": "linux", "restart": "auto" }
    }
  }
}
```

An alias SSH block can be `{ "mode": "alias", "alias": "lab-linux" }`. Authentication stays with OpenSSH. Direct-endpoint readiness checks an SSH banner; proxy aliases are delegated to OpenSSH rather than treating direct TCP polling as route verification.

Writes use validation and conflict detection. Invalid/unreadable originals are not silently replaced by incidental edits. Fix the file or deliberately move your own copy aside, then retry. Preserve recovery copies until understood. Config has no password storage, but usernames and host addresses can still be private: redact before sharing.

## Restart

Configure the guest OS and route using `vbx config`. Routes are `auto`, `ga`, `ssh`, and `off`. Guest Additions requests can fail when unsupported/unavailable. SSH restart currently requires noninteractive authentication and rights to execute its fixed reboot command; normal `vbx ssh` still allows password prompts.

Inspect the result: requested, confirmed command completion, uncertain disconnect, and failure are different outcomes. There is no forced reset/power-cycle fallback.

## Rename and snapshots

```powershell
vbx rename "dev-linux"
vbx rename "dev-linux" "dev-blue"
vbx snapshots "dev-blue"
```

Rename requires a powered-off VM and retains UUID-bound settings. It does not stop the machine for you.

For snapshot create/restore, run `vbx`, choose a VM, then **Snapshots**. Restore requires an eligible stopped machine and offers protection first. If the protective snapshot cannot be created/verified, restore aborts. Changes since the restored snapshot may be lost. Snapshots are not independent backups. Deletion is not implemented.

## Troubleshooting

| Symptom | Next step |
| --- | --- |
| VBoxManage missing | Install VirtualBox or pass `--vboxmanage PATH`. |
| VM absent/ambiguous | Use the same OS user as VirtualBox; check exact name or UUID. |
| SSH timeout | Check the guest SSH service and existing port-forwarding rule. For loopback NAT forwarding use the host address/port, not the guest's private NAT IP. |
| SSH host-key warning | Verify guest identity through a trusted channel; do not disable verification. |
| Guest remains running after stop | Check guest ACPI behavior; the request is not forced power-off. |
| Invalid/conflicting config | Follow the reported path/error, preserve the original, retry. |
| Picker cannot run in a pipe | Supply a direct action and target; interactive flows need a terminal. |

`vbx --help` lists flags, including `--demo`, `--config`, `--vboxmanage`, `--ssh-client`, `--ssh-timeout`, and `--no-color`.

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Successful completion |
| `1` | Failure/cancellation or incomplete batch |
| `2` | Usage/target/interactive-input error |
| `3` | Environment error |

After SSH handoff its exit status is propagated. Read the message as well as the code.
