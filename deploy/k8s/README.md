# deploy/k8s

Minimal Kubernetes manifests that show the **shape** of a production deploy. Not a full Helm chart — just enough to prove the design in `DESIGN.md §12.4`:

- `api-deployment.yaml` — stateless `Deployment` for `notifd api` with an HPA on CPU.
- `worker-deployment.yaml` — stateless `Deployment` for `notifd worker`. Annotated for a queue-depth-based HPA (requires Prometheus Adapter + custom-metrics; stub shown).
- `secret.yaml` — placeholder; production uses External Secrets / SOPS.

These assume a managed Postgres (RDS / Cloud SQL) and managed Redis (ElastiCache / Upstash). That's the explicit trade-off called out in the design doc.
