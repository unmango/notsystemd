# notsystemd

Runs a vendor's systemd service units in containers without systemd, and answers the vendor software's questions about those units the way systemd would.

## Language

**Unit**:
A systemd service unit, as the vendor's `.service` file defines it. notsystemd runs one unit per container.
_Avoid_: service (ambiguous with a Kubernetes Service), job

**Supervisor**:
The `notsystemd run` process that runs one unit and records its state.
_Avoid_: daemon, init

**Shim**:
notsystemd invoked as `systemctl`, `systemd-run`, `timedatectl` or `sudo`, standing in for that command.

**Backend**:
What the shims and the D-Bus server go through to find units, read a unit's state and act on a unit. Running a unit is the supervisor's job and is not behind the backend.
_Avoid_: driver, provider, runtime

**Pod backend**:
The backend that reads unit state from the shared state directory and acts by signalling the supervisor, so it reaches only units in the same pod.

**Kubernetes backend**:
The backend that finds units and acts on them through the Kubernetes API, so it reaches units in other pods.

**Action**:
A request, named by its systemd verb (`restart`, `enable`, `daemon-reload`...), for a backend to change a unit or the manager. Each backend decides what a verb means for it.
_Avoid_: command, operation, job

**Transient run**:
A command started on request, as `systemd-run` does, rather than a unit the pod spec declares. It is not an Action on a unit.

## Relationships

- A **Supervisor** runs exactly one **Unit**.
- A **Shim** reaches **Units** only through a **Backend**, and changes them only by sending it an **Action**.
- The **Pod backend** reaches **Units** in its own pod; the **Kubernetes backend** reaches **Units** across the cluster.

## Flagged ambiguities

- "service" means a systemd unit here and a `Service` object in Kubernetes; say **Unit** for the first.
