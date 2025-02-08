package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/entrepeneur4lyf/nextbase/tools/archive"
	"github.com/entrepeneur4lyf/nextbase/tools/filesystem"
	"github.com/entrepeneur4lyf/nextbase/tools/inflector"
	"github.com/entrepeneur4lyf/nextbase/tools/osutils"
	"github.com/entrepeneur4lyf/nextbase/tools/security"
)

const (
	StoreKeyActiveBackup = "@activeBackup"
)

// CreateBackup creates a new backup of the current app pb_data directory.
//
// If name is empty, it will be autogenerated.
// If backup with the same name exists, the new backup file will replace it.
//
// The backup is executed within a transaction, meaning that new writes
// will be temporary "blocked" until the backup file is generated.
//
// To safely perform the backup, it is recommended to have free disk space
// for at least 2x the size of the pb_data directory.
//
// By default backups are stored in pb_data/backups
// (the backups directory itself is excluded from the generated backup).
//
// When using S3 storage for the uploaded collection files, you have to
// take care manually to backup those since they are not part of the pb_data.
//
// Backups can be stored on S3 if it is configured in app.Settings().Backups.
func (app *BaseApp) CreateBackup(ctx context.Context, name string) error {
	if app.Store().Has(StoreKeyActiveBackup) {
		return errors.New("try again later - another backup/restore operation has already been started")
	}

	app.Store().Set(StoreKeyActiveBackup, name)
	defer app.Store().Remove(StoreKeyActiveBackup)

	event := new(BackupEvent)
	event.App = app
	event.Context = ctx
	event.Name = name
	// default root dir entries to exclude from the backup generation
	event.Exclude = []string{LocalBackupsDirName, LocalTempDirName, LocalAutocertCacheDirName}

	return app.OnBackupCreate().Trigger(event, func(e *BackupEvent) error {
		// generate a default name if missing
		if e.Name == "" {
			e.Name = generateBackupName(e.App, "pb_backup_")
		}

		// make sure that the special temp directory exists
		// note: it needs to be inside the current pb_data to avoid "cross-device link" errors
		localTempDir := filepath.Join(e.App.DataDir(), LocalTempDirName)
		if err := os.MkdirAll(localTempDir, os.ModePerm); err != nil {
			return fmt.Errorf("failed to create a temp dir: %w", err)
		}

		// archive pb_data in a temp directory, exluding the "backups" and the temp dirs
		//
		// Run in transaction to temporary block other writes (transactions uses the NonconcurrentDB connection).
		// ---
		tempPath := filepath.Join(localTempDir, "pb_backup_"+security.PseudorandomString(6))
		createErr := e.App.RunInTransaction(func(txApp App) error {
			return txApp.AuxRunInTransaction(func(txApp App) error {
				// run manual checkpoint and truncate the WAL files
				// (errors are ignored because it is not that important and the PRAGMA may not be supported by the used driver)
				txApp.DB().NewQuery("PRAGMA wal_checkpoint(TRUNCATE)").Execute()
				txApp.AuxDB().NewQuery("PRAGMA wal_checkpoint(TRUNCATE)").Execute()

				return archive.Create(txApp.DataDir(), tempPath, e.Exclude...)
			})
		})
		if createErr != nil {
			return createErr
		}
		defer os.Remove(tempPath)

		// persist the backup in the backups filesystem
		// ---
		fsys, err := e.App.NewBackupsFilesystem()
		if err != nil {
			return err
		}
		defer fsys.Close()

		fsys.SetContext(e.Context)

		file, err := filesystem.NewFileFromPath(tempPath)
		if err != nil {
			return err
		}
		file.OriginalName = e.Name
		file.Name = file.OriginalName

		if err := fsys.UploadFile(file, file.Name); err != nil {
			return err
		}

		return nil
	})
}

// RestoreBackup restores the backup with the specified name and restarts
// the current running application process.
//
// NB! This feature is experimental and currently is expected to work only on UNIX based systems.
//
// To safely perform the restore it is recommended to have free disk space
// for at least 2x the size of the restored pb_data backup.
//
// The performed steps are:
//
//  1. Download the backup with the specified name in a temp location
//     (this is in case of S3; otherwise it creates a temp copy of the zip)
//
//  2. Extract the backup in a temp directory inside the app "pb_data"
//     (eg. "pb_data/.pb_temp_to_delete/pb_restore").
//
//  3. Move the current app "pb_data" content (excluding the local backups and the special temp dir)
//     under another temp sub dir that will be deleted on the next app start up
//     (eg. "pb_data/.pb_temp_to_delete/old_pb_data").
//     This is because on some environments it may not be allowed
//     to delete the currently open "pb_data" files.
//
//  4. Move the extracted dir content to the app "pb_data".
//
//  5. Restart the app (on successful app bootstap it will also remove the old pb_data).
//
// If a failure occure during the restore process the dir changes are reverted.
// If for whatever reason the revert is not possible, it panics.
//
// Note that if your pb_data has custom network mounts as subdirectories, then
// it is possible the restore to fail during the `os.Rename` operations
// (see https://github.com/entrepeneur4lyf/nextbase/issues/4647).
func (app *BaseApp) RestoreBackup(ctx context.Context, name string) error {
	if app.Store().Has(StoreKeyActiveBackup) {
		return errors.New("try again later - another backup/restore operation has already been started")
	}

	app.Store().Set(StoreKeyActiveBackup, name)
	defer app.Store().Remove(StoreKeyActiveBackup)

	event := new(BackupEvent)
	event.App = app
	event.Context = ctx
	event.Name = name
	// default root dir entries to exclude from the backup restore
	event.Exclude = []string{LocalBackupsDirName, LocalTempDirName, LocalAutocertCacheDirName}

	return app.OnBackupRestore().Trigger(event, func(e *BackupEvent) error {
		if runtime.GOOS == "windows" {
			return errors.New("restore is not supported on Windows")
		}

		// make sure that the special temp directory exists
		// note: it needs to be inside the current pb_data to avoid "cross-device link" errors
		localTempDir := filepath.Join(e.App.DataDir(), LocalTempDirName)
		if err := os.MkdirAll(localTempDir, os.ModePerm); err != nil {
			return fmt.Errorf("failed to create a temp dir: %w", err)
		}

		fsys, err := e.App.NewBackupsFilesystem()
		if err != nil {
			return err
		}
		defer fsys.Close()

		fsys.SetContext(e.Context)

		if ok, _ := fsys.Exists(name); !ok {
			return fmt.Errorf("missing or invalid backup file %q to restore", name)
		}

		extractedDataDir := filepath.Join(localTempDir, "pb_restore_"+security.PseudorandomString(8))
		defer os.RemoveAll(extractedDataDir)

		// extract the zip
		if e.App.Settings().Backups.S3.Enabled {
			br, err := fsys.GetFile(name)
			if err != nil {
				return err
			}
			defer br.Close()

			// create a temp zip file from the blob.Reader and try to extract it
			tempZip, err := os.CreateTemp(localTempDir, "pb_restore_zip")
			if err != nil {
				return err
			}
			defer os.Remove(tempZip.Name())
			defer tempZip.Close() // note: this technically shouldn't be necessary but it is here to workaround platforms discrepancies

			_, err = io.Copy(tempZip, br)
			if err != nil {
				return err
			}

			err = archive.Extract(tempZip.Name(), extractedDataDir)
			if err != nil {
				return err
			}

			// remove the temp zip file since we no longer need it
			// (this is in case the app restarts and the defer calls are not called)
			_ = tempZip.Close()
			err = os.Remove(tempZip.Name())
			if err != nil {
				e.App.Logger().Warn(
					"[RestoreBackup] Failed to remove the temp zip backup file",
					slog.String("file", tempZip.Name()),
					slog.String("error", err.Error()),
				)
			}
		} else {
			// manually construct the local path to avoid creating a copy of the zip file
			// since the blob reader currently doesn't implement ReaderAt
			zipPath := filepath.Join(app.DataDir(), LocalBackupsDirName, filepath.Base(name))

			err = archive.Extract(zipPath, extractedDataDir)
			if err != nil {
				return err
			}
		}

		// ensure that at least a database file exists
		extractedDB := filepath.Join(extractedDataDir, "data.db")
		if _, err := os.Stat(extractedDB); err != nil {
			return fmt.Errorf("data.db file is missing or invalid: %w", err)
		}

		// move the current pb_data content to a special temp location
		// that will hold the old data between dirs replace
		// (the temp dir will be automatically removed on the next app start)
		oldTempDataDir := filepath.Join(localTempDir, "old_pb_data_"+security.PseudorandomString(8))
		if err := osutils.MoveDirContent(e.App.DataDir(), oldTempDataDir, e.Exclude...); err != nil {
			return fmt.Errorf("failed to move the current pb_data content to a temp location: %w", err)
		}

		// move the extracted archive content to the app's pb_data
		if err := osutils.MoveDirContent(extractedDataDir, e.App.DataDir(), e.Exclude...); err != nil {
			return fmt.Errorf("failed to move the extracted archive content to pb_data: %w", err)
		}

		revertDataDirChanges := func() error {
			if err := osutils.MoveDirContent(e.App.DataDir(), extractedDataDir, e.Exclude...); err != nil {
				return fmt.Errorf("failed to revert the extracted dir change: %w", err)
			}

			if err := osutils.MoveDirContent(oldTempDataDir, e.App.DataDir(), e.Exclude...); err != nil {
				return fmt.Errorf("failed to revert old pb_data dir change: %w", err)
			}

			return nil
		}

		// restart the app
		if err := e.App.Restart(); err != nil {
			if revertErr := revertDataDirChanges(); revertErr != nil {
				panic(revertErr)
			}

			return fmt.Errorf("failed to restart the app process: %w", err)
		}

		return nil
	})
}

// registerAutobackupHooks registers the autobackup app serve hooks.
func (app *BaseApp) registerAutobackupHooks() {
	const jobId = "__pbAutoBackup__"

	loadJob := func() {
		rawSchedule := app.Settings().Backups.Cron
		if rawSchedule == "" {
			app.Cron().Remove(jobId)
			return
		}

		app.Cron().Add(jobId, rawSchedule, func() {
			const autoPrefix = "@auto_pb_backup_"

			name := generateBackupName(app, autoPrefix)

			if err := app.CreateBackup(context.Background(), name); err != nil {
				app.Logger().Error(
					"[Backup cron] Failed to create backup",
					slog.String("name", name),
					slog.String("error", err.Error()),
				)
			}

			maxKeep := app.Settings().Backups.CronMaxKeep

			if maxKeep == 0 {
				return // no explicit limit
			}

			fsys, err := app.NewBackupsFilesystem()
			if err != nil {
				app.Logger().Error(
					"[Backup cron] Failed to initialize the backup filesystem",
					slog.String("error", err.Error()),
				)
				return
			}
			defer fsys.Close()

			files, err := fsys.List(autoPrefix)
			if err != nil {
				app.Logger().Error(
					"[Backup cron] Failed to list autogenerated backups",
					slog.String("error", err.Error()),
				)
				return
			}

			if maxKeep >= len(files) {
				return // nothing to remove
			}

			// sort desc
			sort.Slice(files, func(i, j int) bool {
				return files[i].ModTime.After(files[j].ModTime)
			})

			// keep only the most recent n auto backup files
			toRemove := files[maxKeep:]

			for _, f := range toRemove {
				if err := fsys.Delete(f.Key); err != nil {
					app.Logger().Error(
						"[Backup cron] Failed to remove old autogenerated backup",
						slog.String("key", f.Key),
						slog.String("error", err.Error()),
					)
				}
			}
		})
	}

	app.OnBootstrap().BindFunc(func(e *BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}

		loadJob()

		return nil
	})

	app.OnSettingsReload().BindFunc(func(e *SettingsReloadEvent) error {
		if err := e.Next(); err != nil {
			return err
		}

		loadJob()

		return nil
	})
}

func generateBackupName(app App, prefix string) string {
	appName := inflector.Snakecase(app.Settings().Meta.AppName)
	if len(appName) > 50 {
		appName = appName[:50]
	}

	return fmt.Sprintf(
		"%s%s_%s.zip",
		prefix,
		appName,
		time.Now().UTC().Format("20060102150405"),
	)
}
