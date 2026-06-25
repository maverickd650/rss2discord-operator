---
name: types-update
description: After editing *_types.go or kubebuilder markers, regenerate CRDs, DeepCopy, lint, and run tests in the correct order
disable-model-invocation: true
---

Run this after any change to `api/**/*_types.go` or kubebuilder markers (`// +kubebuilder:...`):

```bash
mise run manifests   # regenerate CRDs and RBAC from markers
mise run generate    # regenerate DeepCopy methods (zz_generated.*.go)
mise run lint-fix    # auto-fix code style
mise run test        # run unit tests
```

Then verify `config/crd/bases/` reflects the expected schema changes before committing.

**Do not skip steps** — `manifests` and `generate` produce separate outputs that both must be up to date.
