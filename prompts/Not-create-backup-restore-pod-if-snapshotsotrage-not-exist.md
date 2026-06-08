Referred YAMLS Directory

/home/anisur/go/src/github.com/anisurrahman75/kubestash-notes/volume-snapshots/topolvm/ 





Suppose, DUring Online mode VolumeSnapshot taking, 

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotClass
metadata:
  name: topolvm-online
driver: topolvm.io
deletionPolicy: Delete
parameters:
  topolvm.io/snapshotMode: "online"
  topolvm.io/snapshotStorageNamespace: "topolvm-system"
  topolvm.io/snapshotStorageName: "s3-storage"
```

User refere volumesnapshotclass name in VolumeSnapshot. 

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: my-snapshot
  namespace: demo
spec:
  volumeSnapshotClassName: topolvm-online
  source:
    persistentVolumeClaimName: my-pvc
```

If the snapshotStorage doesn't exist, backup-restore pod will not be created, and the VolumeSnapshot will be in `Error` state with message `snapshot storage s3-storage not found in namespace topolvm-system`. 

It'll only happen if SnapshotMode is `online`, because `offline` mode doesn't need snapshot storage.


