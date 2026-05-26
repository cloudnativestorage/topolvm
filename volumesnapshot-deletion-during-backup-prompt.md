# Prompt: Handle VolumeSnapshot Deletion During Ongoing Backup

## Scenario

1. A user creates a `VolumeSnapshot` for a PVC.
2. A **backup job/pod** is spawned that runs the `restic backup` command (via `topolvm-snapshotter backup`).
3. While the backup is still **in progress**, the user deletes the `VolumeSnapshot`.
4. As a result:
   - The reconcilatio of logicalVolume CR wait until the Actual LV deletetion, 
   - But Actual linux LV will not be deleted cause a backup Pod is running, 

5. Solution (Thinking only, might be cahange):
   -  During Delete, First Delete the Backup Pod
   - Next No Pod is using this Linux LV. 
   - Next, Delete actual Linux LV
   - Next Delete the LogicalVolume  
