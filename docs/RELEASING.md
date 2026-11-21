# Release checklist

This prepares a release; it does not imply one has been published.

1. Review the intended diff and version in `internal/version`. Preserve unrelated work.
2. Run formatting, vet, tests and Windows builds. Smoke-test help/version. Inspect demo Escape/Ctrl+C, rename, SSH setup, batches and snapshots.
3. Run `govulncheck ./...` with trusted tooling and advisory access. Record actual results/toolchain/blockers; review dependency updates.
4. Test authorized real-VM journeys separately. Record platforms and VirtualBox versions actually verified.
5. Check intended files/artifacts for credentials, production config/inventories, personal contacts/paths and real-session captures. Use synthetic examples.
6. **Review all Git history**, including author/committer metadata. Cleaning files does not erase old commits. Use a public handle and verified GitHub noreply identity for future commits. Sensitive historical metadata needs an explicit cleanup plan; do not force-push or rewrite it incidentally.
7. Enable private vulnerability reporting if desired and verify `SECURITY.md` links. Review Actions permissions, fork behavior and branch protection rather than assuming settings.
8. Build with `-trimpath`, inspect module/build metadata, exclude config/logs, and publish checksums with binary downloads. Do not claim signatures unless artifacts are signed.
9. Prepare notes covering compatibility/limitations and verify links. Publish/tag/upload only with authorization.

CI artifacts are development builds. Package-manager distribution, signing and release automation remain separate decisions.
