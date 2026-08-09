# Deploying tehran on k3s

Plain manifests plus Kustomize. Nothing here needs Helm, and the only tool
required is `kubectl` — its built-in Kustomize handles `apply -k`.

The image comes from GHCR, built and pushed by the `tag` workflow. Nothing is
built at deploy time.

## Layout

| Path | What it is |
|---|---|
| `kustomization.yaml` | the app: namespace, config, Deployment, Service, Ingress |
| `migrate/` | the migration Job, applied on its own so it can be waited on |
| `release/` | a Component holding the image tag — the only place a version is named |
| `secret.example.yaml` | placeholders for reference; not applied, not real |

## First deploy

**1. Create the database Secret.** It is deliberately not in this repo:

```sh
# Creates the namespace declaratively. `kubectl create namespace` also works,
# but every later `apply` then warns about the annotation it did not write.
kubectl apply -k deploy/k8s/base

kubectl create secret generic tehran-database -n tehran \
  --from-literal=TEHRAN_DATABASE_HOST=<postgres host> \
  --from-literal=TEHRAN_DATABASE_PORT=5432 \
  --from-literal=TEHRAN_DATABASE_USER=<user> \
  --from-literal=TEHRAN_DATABASE_PASSWORD=<password> \
  --from-literal=TEHRAN_DATABASE_DATABASE=<database>
```

For a Postgres inside the cluster, the host is its Service DNS name —
`postgres.default.svc.cluster.local`, or the short `postgres` from within the
same namespace.

Until this Secret exists the pods stay in `CreateContainerConfigError`, which
is the correct failure: no credentials, no start.

**2. Set the hostname.** `ingress.yaml` ships with a `nip.io` name, which
resolves `*.<ip>.nip.io` to that IP with no DNS to configure. Replace it with a
real name when there is one.

**3. Deploy.**

```sh
make k8s-deploy
```

## Releasing a new version

Edit the tag in `release/kustomization.yaml` — one line, one place — then:

```sh
make k8s-deploy
```

The migration Job and the Deployment take the tag from that Component, so the
version that migrates the schema is by construction the version that serves it.

Use `make k8s-diff` first to see what would change.

## Why the deploy is four commands and not one

`make k8s-deploy` runs:

```sh
kubectl delete job tehran-migrate -n tehran --ignore-not-found
kubectl apply -k deploy/k8s/migrate
kubectl wait --for=condition=complete job/tehran-migrate -n tehran --timeout=300s
kubectl apply -k deploy/k8s
kubectl rollout status deployment/tehran -n tehran --timeout=300s
```

Both odd-looking steps are load-bearing:

- **The delete.** A Job's pod template is immutable. Re-applying one after a
  release bump fails with `field is immutable`, so the old Job goes first.
- **The split apply.** Applying the Job and the Deployment together starts the
  rollout while the migration is still running. Nothing downstream would catch
  the resulting window, because `/readyz` checks the database *connection* and
  not the schema — it answers `200` against a completely unmigrated database.
  A pod can be Ready and still fail every campaign RPC.

If the migration fails, `kubectl wait` fails and the app is never applied — the
running version keeps serving, and the Job's logs are printed so the reason is
in front of you rather than one `kubectl logs` away.

`kubectl wait` cannot watch for completion and failure at once, so a failing
migration spends the full timeout on its retries first. Shorten it when
iterating on a broken migration:

```sh
make k8s-deploy K8S_TIMEOUT=45s
```

## Ports

| Port | Serves | Exposed |
|---|---|---|
| 8080 | Connect RPC | via the Ingress |
| 9090 | `/healthz`, `/readyz`, `/metrics` | cluster-internal only |

The Ingress routes 8080 and nothing else. For metrics, use the Service —
`kubectl port-forward svc/tehran 9090:9090 -n tehran` — or point a scraper at
it in-cluster.

Connect's protocol and its JSON form are ordinary HTTP/1.1 POSTs, so the
Ingress serves them as shipped. gRPC clients need h2c to the backend, which on
Traefik means adding to `ingress.yaml`:

```yaml
metadata:
  annotations:
    traefik.ingress.kubernetes.io/service.serversscheme: h2c
```

## Checking it works

```sh
kubectl get pods -n tehran
kubectl logs -n tehran deployment/tehran

curl -X POST https://<host>/proto.campaign.v1.CampaignService/CreateCampaign \
  -H 'Content-Type: application/json' \
  -d '{"name":"first","description":"hello"}'
```

A campaign with a UUID back means image, config, Secret, migrations, Service
and Ingress are all correct.

## Notes

- **Postgres is not deployed here.** These manifests describe the app and
  nothing else; point the Secret at whatever database you run.
- **Editing `configmap.yaml` does not restart anything.** `envFrom` is read at
  container start. Follow it with
  `kubectl rollout restart deployment/tehran -n tehran`. Same for rotating the
  Secret.
- **`runAsUser: 65532` is repeated from the image.** The image's own user is
  the name `nonroot`, and `runAsNonRoot: true` cannot verify a name — the
  kubelet rejects the pod outright. The numeric UID has to be stated.
