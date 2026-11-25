# Custos

**Custos**: the guardian, the one an alchemist trusts to keep the credential config and let no one at it who has no business with it.

A `CredentialConfig` says which values a service's credentials are made of and where each one comes from. Custos collects them, renders them and writes one Secret where the consumer can reach it.

Documentation: https://docs.helmetica.io/custos/

## Development

```bash
just test
just run    # against the cluster in your KUBECONFIG
just docs   # preview the docs
```
