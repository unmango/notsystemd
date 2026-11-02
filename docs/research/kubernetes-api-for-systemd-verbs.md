# Kubernetes API counterparts for the systemd verbs notsystemd answers

Research for [unmango/notsystemd#12](https://github.com/unmango/notsystemd/issues/12): if the `systemctl` and `systemd-run` shims and the D-Bus server answered from the Kubernetes API instead of the per-unit state files (`internal/state/state.go`), what does each operation map to, and what are the limits?
Facts only; no design decisions.

Gathered 2026-11-02.
The newest released Kubernetes is v1.37 (kubernetes.io's version picker); v1.38 is in development.
Feature stages come from the `kubernetes/website` feature-gate files (rendered at [Feature Gates](https://kubernetes.io/docs/reference/command-line-tools-reference/feature-gates/)) and the KEPs' `kep.yaml`.
Where a docs page and the code disagree, both are quoted.

## 1. Reading state

### What the API exposes

Pod phase ([Pod Lifecycle: Pod phase](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-phase)) is a pod-wide summary with exactly five values, and the docs warn that "nothing should be assumed about Pods that have a given `phase` value" beyond what is documented:

| phase | meaning (quoted/condensed from the docs) |
|---|---|
| `Pending` | accepted, but one or more containers not yet set up and made ready to run; includes scheduling and image pulls |
| `Running` | bound to a node, all containers created, at least one still running "or is in the process of starting or restarting" |
| `Succeeded` | all containers terminated in success and will not be restarted |
| `Failed` | all containers terminated, at least one in failure, and not set for automatic restart |
| `Unknown` | state could not be obtained, typically node communication failure |

`CrashLoopBackOff` and `Terminating` are kubectl display strings, not phases (same page, note under Pod phase).
Since 1.27 the kubelet moves a deleted pod to `Failed`/`Succeeded` before it is removed from the API server, except static pods and force-deleted pods.

Per-container status, `status.containerStatuses[]` (and `initContainerStatuses`, `ephemeralContainerStatuses`), from [`ContainerStatus` in `k8s.io/api/core/v1/types.go`](https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/api/core/v1/types.go) and [Container states](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#container-states):

- `state`: exactly one of `waiting` (`reason`, `message`), `running` (`startedAt`), `terminated` (`exitCode`, `signal`, `reason`, `message`, `startedAt`, `finishedAt`, `containerID`). None set means waiting.
  `running` means any `postStart` hook has already finished; `terminated` is entered after any `preStop` hook ran.
- `lastState`: the previous termination, "not populated if the container is still running and RestartCount is 0". This is where the exit code of the run before a restart lives.
- `ready`: currently passing its readiness check; with no readiness probe, true once `started`.
- `started`: true once `postStart` finished and the startup probe passed; reset to false on restart or when the kubelet loses state. Null must be read as false.
- `restartCount`: "Kubelet makes an effort to always increment the value, but … the value may be reset to 0" after node restarts.
- `stopSignal`: the effective stop signal (with the `ContainerStopSignals` gate).

Pod conditions ([Pod conditions](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-conditions)): `PodScheduled`, `PodReadyToStartContainers` (GA in v1.37), `Initialized`, `ContainersReady`, `Ready`, `DisruptionTarget` (about to be terminated by preemption, eviction or GC), `PodResizePending`, `PodResizeInProgress`, plus `AllContainersRestarting` for in-place pod restarts (section 2).
Each has `status` (`True`/`False`/`Unknown`), `reason`, `message`, `lastTransitionTime`.
`readinessGates` let a third party add conditions that `Ready` waits for; conditions are written with a PATCH to the `status` subresource ([Pod readiness](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-readiness-gate)).

`metadata.deletionTimestamp` being set is what kubectl shows as `Terminating` ([Pod termination flow](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-termination-flow)).

### Which fields carry the information systemd's properties carry

notsystemd's state file records `ActiveState`, `SubState`, `StatusText`, `MainPID`, `Result` and `ExitStatus`.
The table lists the Kubernetes field holding comparable information for a unit that is one container; it is a correspondence of facts, not a chosen mapping.

| systemd property | closest Kubernetes information | gaps |
|---|---|---|
| `ActiveState=activating` | container `state.waiting`, or `running` with `started=false`; pod `Pending` | no distinction between ExecStartPre and the main process |
| `ActiveState=active` | `state.running` and `started=true` (or `ready=true` if readiness is what "active" should mean) | Kubernetes has no READY=1 equivalent other than a readiness/startup probe |
| `ActiveState=reloading` | none | nothing in Pod status represents a reload |
| `ActiveState=deactivating` | `metadata.deletionTimestamp` set (whole pod), `AllContainersRestarting=True` | a single container stopping before restart has no observable "stopping" state |
| `ActiveState=inactive` / `failed` | `state.terminated` with `exitCode` 0 / non-zero; pod `Succeeded` / `Failed` | under `restartPolicy: Always` a terminated container shows `waiting` (`CrashLoopBackOff`) with the exit in `lastState`, not a lasting inactive/failed |
| `SubState` | `state.waiting.reason` (e.g. `CrashLoopBackOff`), `state.terminated.reason` (e.g. `OOMKilled`) | free-form reason strings, not an enum |
| `Result` | `terminated.reason` / `exitCode` / `signal`; probe kills appear as events and restarts | no `timeout` or `watchdog` result; a liveness-probe kill is reported as a termination like any other |
| `ExecMainStatus` | `state.terminated.exitCode`, else `lastState.terminated.exitCode` | |
| `StatusText` | none in Pod status; a custom pod condition's `message`, or an annotation, could carry text | requires `pods/status` patch rights |
| `MainPID` | none | PIDs are not in the API; `containerID` is the runtime's ID |
| `NRestarts` (systemd) | `restartCount` | may reset to 0 after node restarts |

### How changes are delivered

[API Concepts: Efficient detection of changes](https://kubernetes.io/docs/reference/using-api/api-concepts/#efficient-detection-of-changes):

- A client lists or gets, takes `metadata.resourceVersion`, and issues `GET …/pods?watch=1&resourceVersion=N`. The response is a chunked stream of JSON documents `{"type": ADDED|MODIFIED|DELETED|BOOKMARK|ERROR, "object": …}`. Every status change is a `MODIFIED` of the whole Pod.
- History is kept for a limited time ("Clusters using etcd 3 preserve changes in the last 5 minutes by default"). A watch from a too-old version gets `410 Gone`; the client must re-list and watch again.
- `allowWatchBookmarks=true` adds `BOOKMARK` events carrying only a `resourceVersion`, to resume without re-listing ([Watch bookmarks](https://kubernetes.io/docs/reference/using-api/api-concepts/#watch-bookmarks)).
- A single pod is watched with a `metadata.name` field selector. If RBAC restricts `list`/`watch` by `resourceNames`, the client **must** send that field selector ([RBAC note](https://kubernetes.io/docs/reference/access-authn-authz/rbac/#referring-to-resources)).
- client-go's `Reflector`/informers implement list-then-watch (same page). Adopting client-go would be the first non-stdlib dependency, which AGENTS.md rules out; the REST/JSON watch is usable with `net/http` alone.

## 2. Restart, stop, start

### Updating a running Pod in place: what the API allows

[Pod update and replacement](https://kubernetes.io/docs/concepts/workloads/pods/#pod-update-and-replacement): a Pod update may only change `spec.containers[*].image`, `spec.initContainers[*].image`, `spec.activeDeadlineSeconds`, `spec.terminationGracePeriodSeconds`, `spec.tolerations` (add only) and `spec.schedulingGates`.
Subresources add `resize` (container resources), `ephemeralcontainers`, `status` and `binding`.
There is **no subresource or verb that restarts or stops a container on request.**

### Ways a single container gets restarted without deleting the Pod

| mechanism | triggered by | stage | notes |
|---|---|---|---|
| Pod `restartPolicy` (`Always`/`OnFailure`/`Never`) | the container exiting | GA | exponential backoff, default 10s doubling to 300s, reset after a successful run (~10 min) ([Container restarts](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#restart-policy)) |
| Container-level `restartPolicy` + `restartPolicyRules` ([KEP-5307](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/5307-container-restart-policy), gate `ContainerRestartRules`) | the container exiting with a matching exit code | alpha 1.34, **beta (on by default) 1.35**, still beta in 1.37; KEP lists GA target 1.36, not reached | action `Restart`; condition `exitCodes` with `In`/`NotIn`; up to 20 rules; not for ephemeral containers; same backoff ([docs](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#container-restart-rules)) |
| `RestartAllContainers` action ([KEP-5532](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/5532-restart-all-containers-on-container-exits), gate `RestartAllContainersOnContainerExits`) | a container exiting with a matching exit code | alpha 1.35, **beta (on by default) 1.36**; GA target 1.37 per KEP, gate still beta | restarts every container in place, re-running init containers; keeps pod UID, IP, sandbox, volumes (`emptyDir` included); skips `terminationGracePeriodSeconds` and `preStop`; depends on `ContainerRestartRules` and `NodeDeclaredFeatures` ([docs](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#restart-all-containers)) |
| Liveness / startup probe failure | the kubelet's probe | GA | kills the container, then the restart policy applies ([Container probes](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#container-probes)) |
| Changing `spec.containers[*].image` | a Pod PATCH | GA (the field is mutable) | kubelet: `containerChanged` hashes `image` and `name`; on change, "Restart regardless of the restart policy because the container spec changed" ([kuberuntime_manager.go](https://github.com/kubernetes/kubernetes/blob/master/pkg/kubelet/kuberuntime/kuberuntime_manager.go)). Patching the same value is a no-op; not documented on kubernetes.io as a restart mechanism |
| In-place resize with `resizePolicy[].restartPolicy: RestartContainer` ([KEP-1287](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/1287-in-place-update-pod-resources), gate `InPlacePodVerticalScaling`) | a PATCH to `pods/resize` that actually changes CPU or memory | alpha 1.27, beta 1.33, **GA 1.35** | restarts only that container; requires a real resource change; not allowed (must be `NotRequired`) when pod `restartPolicy: Never` ([Resize container resources](https://kubernetes.io/docs/tasks/configure-pod-container/resize-container-resources/)) |

Both KEPs state that triggering a restart directly through the API is out of scope.
KEP-5307's restarts are only on container exit; KEP-5532's Non-Goals: "Introducing triggers for pod restart other than container exits (e.g., via a direct API call). This could be a future enhancement."

Name discrepancy: the docs page says the in-place restart sets a `PodRestartInPlace` condition; KEP-5532 and the API constant (`AllContainersRestarting PodConditionType = "AllContainersRestarting"` in `core/v1/types.go`) use `AllContainersRestarting`.

Inconsistency caveat from the docs: kubelet restarts, runtime GC and control-plane connectivity loss "may cause the state loss and containers may be re-run even when you expect a container not to be restarted".

### Ways that replace the whole Pod

- **Delete the Pod** (`DELETE …/pods/NAME`): graceful by default (30s `terminationGracePeriodSeconds`); `preStop` hooks run, then the stop signal to PID 1 of each container, SIGKILL at the deadline ([Pod termination](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-termination)). The controller (Deployment/StatefulSet/Job) creates a replacement with a new UID and IP; `emptyDir` is lost. A bare Pod is not recreated. Sidecars are stopped after the main containers, in reverse order ([Pod shutdown and sidecars](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#termination-with-sidecars)).
- **Eviction** (`POST …/pods/NAME/eviction`, RBAC `create` on `pods/eviction`): a delete that honours PodDisruptionBudgets ([API-initiated eviction](https://kubernetes.io/docs/concepts/scheduling-eviction/api-eviction/)).
- **`kubectl rollout restart`**: patches `spec.template.metadata.annotations["kubectl.kubernetes.io/restartedAt"]` with the current time on a Deployment, StatefulSet or DaemonSet, so the controller rolls out new Pods per its update strategy. Setting it on a Pod does nothing ([well-known annotations](https://kubernetes.io/docs/reference/labels-annotations-taints/#kubectl-k8s-io-restart-at), [kubectl rollout restart](https://kubernetes.io/docs/reference/kubectl/generated/kubectl_rollout/kubectl_rollout_restart/)). Restarts every replica, all containers.
- **Scale to 0 and back** (the `scale` subresource, as `kubectl scale` uses; [Deployments: scaling](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/#scaling-a-deployment)): stops all replicas; the closest analogue to `systemctl stop` that persists. Nothing restarts it until something scales it up again.

### `start`

A Pod's containers cannot be added or started after creation except ephemeral containers ([Ephemeral containers](https://kubernetes.io/docs/concepts/workloads/pods/ephemeral-containers/)).
"Start" therefore exists only at workload level: create a Pod/Job, or scale a Deployment/StatefulSet above 0.

## 3. Reload and kill: signalling a process

- There is no API to send an arbitrary signal to a container. Kubernetes signals only during termination: the image's `STOPSIGNAL` or the runtime default (SIGTERM for containerd and CRI-O), then SIGKILL ([Stop signals](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-termination-stop-signals)).
- `lifecycle.stopSignal` ([KEP-4960](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/4960-container-stop-signals), gate `ContainerStopSignals`) picks the stop signal per container (needs `spec.os.name`). Alpha since 1.33, beta targeted for 1.38. Still a termination-time signal only.
- **`exec`** (`pods/exec`): runs a command in the target container, so `kill -HUP 1` works only if the image has a `kill`. Since 1.35 (`AuthorizePodWebsocketUpgradeCreatePermission`, beta), WebSocket exec also requires `create` on `pods/exec` ([feature gate](https://kubernetes.io/docs/reference/command-line-tools-reference/feature-gates/)).
- **Ephemeral container with `targetContainerName`**: joins the target container's PID (and IPC) namespace, so it can signal the target's processes without `shareProcessNamespace` and without a shell in the target image. Needs runtime support ("undefined" otherwise), `update`/`patch` on `pods/ephemeralcontainers` (not in the default `admin`/`edit` roles), and cannot be removed once added: it stays in the Pod spec until the Pod is deleted ([Ephemeral containers](https://kubernetes.io/docs/concepts/workloads/pods/ephemeral-containers/), [Debugging with an ephemeral container](https://kubernetes.io/docs/tasks/debug/debug-application/debug-running-pod/#ephemeral-container)). GA since 1.25.
- **`shareProcessNamespace: true`**: only within one Pod. PID 1 becomes the pause container, so `kill -HUP 1` hits the sandbox; processes and `/proc/$pid/root` filesystems are visible across containers ([Share process namespace](https://kubernetes.io/docs/tasks/configure-pod-container/share-process-namespace/)).
- Nothing reaches a process **in another Pod** except `exec` or an ephemeral container added to that Pod.
- A signal can be produced indirectly: delete the Pod (stop signal), a failing liveness probe (kill and restart), or an image change (kill and restart). None of these is SIGHUP-to-reload.

## 4. Transient runs (`systemd-run`)

| target | lifecycle | following it |
|---|---|---|
| **Job** (`batch/v1`) | the Job controller creates Pods and retries per `backoffLimit` (default 6) and `podFailurePolicy`; terminal conditions `Complete`/`Failed`, which since 1.31 are added only once all Job Pods have terminated, with `SuccessCriteriaMet`/`FailureTarget` marking the earlier decision; `ttlSecondsAfterFinished` deletes it afterwards ([Jobs](https://kubernetes.io/docs/concepts/workloads/controllers/job/)) | watch the Job for a terminal condition and its Pod (label `batch.kubernetes.io/job-name`) for exit code and logs |
| **Bare Pod** with `restartPolicy: Never` | runs once; not recreated if its node fails; docs recommend a Job "even if your application requires only a single Pod" ([Bare Pods](https://kubernetes.io/docs/concepts/workloads/controllers/job/#bare-pods)) | watch until phase `Succeeded`/`Failed`, read `state.terminated.exitCode`, delete it yourself |
| **Ephemeral container** in an existing Pod | runs in a Pod that already exists, sharing its network and volumes it mounts; never restarted; no probes, ports or resources; cannot be removed after completion ([Ephemeral containers](https://kubernetes.io/docs/concepts/workloads/pods/ephemeral-containers/)) | watch `ephemeralContainerStatuses` |

Caveats from the Job page: even `parallelism: 1`, `completions: 1`, `restartPolicy: Never` "may sometimes be started twice", and `OnFailure` re-runs the container in the same Pod.

`--wait` / `--pipe` counterparts:

- `--wait` (block, return the exit status): watch the Pod (or Job) until the container is `terminated`, then read `exitCode`.
- Output: `GET …/pods/NAME/log?container=C&follow=true` streams stdout/stderr (`PodLogOptions`: `container`, `follow`, `previous`, `sinceTime`, `stream`, in `core/v1/types.go`); RBAC `get` on `pods/log`. The log stream merges stdout and stderr unless `stream` selects one.
- `--pipe` stdin: the container needs `stdin: true` (and usually `stdinOnce: true`, which closes stdin after the first attach session disconnects), then `pods/attach` (RBAC `create`) carries stdin and stdout/stderr. With `stdinOnce` the container sees EOF when the attach ends.
- `exec` into an already-running container is the other way to run a command transiently and get its exit status and streams, without creating an object (`create` on `pods/exec`).

## 5. In-cluster access

[Accessing the Kubernetes API from a Pod](https://kubernetes.io/docs/tasks/run-application/access-api-from-pod/):

- Endpoint: `https://$KUBERNETES_SERVICE_HOST:$KUBERNETES_SERVICE_PORT_HTTPS`. The control plane "is expected to present a valid certificate for the hostname or IP address that `$KUBERNETES_SERVICE_HOST` represents"; `kubernetes.default.svc` is also published but its certificate is not guaranteed.
- Files under `/var/run/secrets/kubernetes.io/serviceaccount/`: `token` (bearer token), `ca.crt` (bundle to verify the API server, from the `kube-root-ca.crt` ConfigMap), `namespace`.
- The token is projected by the kubelet through the TokenRequest API, bound to the Pod, audience kube-apiserver, default lifetime 1 hour, invalid once the Pod is deleted ([Bound service account token volume](https://kubernetes.io/docs/reference/access-authn-authz/service-accounts-admin/#bound-service-account-token-volume)). The kubelet rotates it when older than 80% of its TTL or 24 hours, and "The application is responsible for reloading the token when it rotates", e.g. re-reading it every 5 minutes ([Configure service accounts](https://kubernetes.io/docs/tasks/configure-pod-container/configure-service-account/#launch-a-pod-using-service-account-token-projection)). Custom projections take `expirationSeconds` ≥ 600 ([Projected volumes](https://kubernetes.io/docs/concepts/storage/projected-volumes/)).
- `automountServiceAccountToken: false` on the Pod or ServiceAccount disables the mount ([Service accounts](https://kubernetes.io/docs/concepts/security/service-accounts/)).
- `rest.InClusterConfig()` in client-go does the above automatically; doing it by hand needs only `net/http` and `crypto/tls`.

RBAC verbs come from HTTP methods: POST → `create`, GET → `get`/`list`/`watch`, PUT → `update`, PATCH → `patch`, DELETE → `delete` ([Request verbs](https://kubernetes.io/docs/reference/access-authn-authz/authorization/#determine-the-request-verb)).
`resourceNames` can restrict `get`/`update`/`patch`/`delete`, and subresource `create` such as `pods/exec`, but not top-level `create`; restricting `list`/`watch` requires the client to send a `metadata.name` field selector ([RBAC](https://kubernetes.io/docs/reference/access-authn-authz/rbac/#referring-to-resources)).

Minimal grants per operation (core group `""` unless noted):

| operation | resource | verbs |
|---|---|---|
| read state once | `pods` | `get` (or `list` with a selector) |
| follow state | `pods` | `list`, `watch` (one pod via name field selector) |
| restart/stop by deleting the Pod | `pods` | `delete` |
| delete honouring PDBs | `pods/eviction` | `create` |
| rollout restart | `deployments`/`statefulsets`/`daemonsets` (`apps`) | `patch` (kubectl also `get`s) |
| scale to 0 and back | `deployments/scale`/`statefulsets/scale` (`apps`) | `get`, `patch` or `update` |
| restart one container via resize | `pods/resize` | `patch` |
| restart one container via image change | `pods` | `patch` |
| reload/kill via exec | `pods/exec` | `create` (WebSocket also checked as `create` since 1.35) |
| signal via ephemeral container | `pods/ephemeralcontainers` | `patch`/`update` |
| transient run as Job | `jobs` (`batch`) | `create`, `get`/`watch`, `delete` if cleaning up |
| transient run as Pod | `pods` | `create`, `get`/`watch`, `delete` |
| output | `pods/log` | `get` |
| stdin | `pods/attach` | `create` |
| custom status text in a condition | `pods/status` | `patch` |

`get`, `list` and `watch` all return full objects and are equivalent in data exposure ([Authorization caution](https://kubernetes.io/docs/reference/access-authn-authz/authorization/#determine-the-request-verb)).
