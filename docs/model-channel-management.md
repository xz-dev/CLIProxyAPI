# Model channel configuration recovery

The model-channel management endpoints update only one channel's `models` node and keep `config.yaml` on the same inode. Before each write, CLIProxyAPI stores the complete pre-write bytes at:

```text
<config.yaml>.model-channel.bak
```

The in-place write is protected against ordinary errors, but it is not crash-atomic. A process or host crash between truncate/write/fsync operations can leave `config.yaml` incomplete.

## Recovery

1. Stop CLIProxyAPI before inspecting or restoring either file.
2. Inspect `config.yaml` and `.model-channel.bak` without printing secrets. Validate the backup with the same CLIProxyAPI version when possible.
3. Restore backup bytes into the existing config inode so mode, owner, and bind-mount identity remain unchanged:

   ```bash
   cp --preserve=mode,ownership config.yaml config.yaml.recovery-before
   cat config.yaml.model-channel.bak > config.yaml
   sync
   ```

4. Validate YAML and expected channel/model selectors, then restart CLIProxyAPI.
5. Verify management inventory and client model catalogs before re-enabling scheduled membership or metadata synchronization.

## Backup lifecycle

The backup is overwritten before every model-channel write and always contains bytes from immediately before that attempted write. Keep it through post-write reload and catalog validation. After validation it may remain in place for the next write; do not treat it as a historical backup or copy it to logs. Production rollout backups remain separate and authoritative for full rollback.
