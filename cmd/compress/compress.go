/*
Check the list of photos to list and discard duplicates.
*/
package compress

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"time"

	"github.com/simulot/immich-go/browser"
	"github.com/simulot/immich-go/cmd"

	"github.com/simulot/immich-go/helpers/myflag"
	"github.com/simulot/immich-go/immich"
)

type CompressCmd struct {
	*cmd.SharedFlags
	AssumeYes    bool             // When true, doesn't ask to the user
	DateRange    immich.DateRange // Set capture date range
	dirConverted *string          // Keep converted files for retry
}

type duplicateKey struct {
	Date time.Time
	Name string
	Type string
}

func NewCompressCmd(ctx context.Context, common *cmd.SharedFlags, args []string) (*CompressCmd, error) {
	cmd := flag.NewFlagSet("compress", flag.ExitOnError)
	validRange := immich.DateRange{}
	_ = validRange.Set("1850-01-04,2030-01-01")
	app := CompressCmd{
		SharedFlags:  common,
		DateRange:    validRange,
		dirConverted: nil,
	}

	app.SharedFlags.SetFlags(cmd)

	cmd.BoolFunc("yes", "When true, assume Yes to all actions", myflag.BoolFlagFn(&app.AssumeYes, false))
	cmd.Var(&app.DateRange, "date", "Process only documents having a capture date in that range.")
	app.dirConverted = cmd.String("dir", "./immich-compressed", "Save here compressed and sendet files fo future retries")

	err := cmd.Parse(args)
	if err != nil {
		return nil, err
	}
	err = app.SharedFlags.Start(ctx)
	if err != nil {
		return nil, err
	}
	return &app, err
}

func getFileSize(filePath string) (int64, error) {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return 0, err
	}
	return fileInfo.Size(), nil
}

func bytesToMB(bytes int64) float64 {
	return float64(bytes) / math.Pow(1024, 2)
}

func compresFile(ctx context.Context, asset *immich.Asset, app *CompressCmd) error {
	oldFileName := filepath.Base(asset.OriginalPath)
	baseNameWithoutExt := strings.TrimSuffix(oldFileName, filepath.Ext(oldFileName))
	var extensionNew string
	switch asset.Type {
	case "IMAGE":
		extensionNew = "jxl"
	case "VIDEO":
		extensionNew = "mp4"
	default:
		return fmt.Errorf("we do not support type: %s", asset.Type)
	}
	newFileName := baseNameWithoutExt + "." + extensionNew

	convertedDir := filepath.Join(*app.dirConverted, "converted")
	directoryPath := filepath.Dir(asset.OriginalPath)
	destDir := filepath.Join(convertedDir, directoryPath)
	destFile := filepath.Join(destDir, newFileName)

	if asset.DeviceID == "immich-go-compress" {
		fmt.Printf("-- was done before device_id: %s \n", asset.OriginalFileName)
		return nil
	}
	_, err := os.Stat(destFile)
	if err == nil {
		fmt.Printf("-- was done before file: %s \n", asset.OriginalFileName)
		return nil
	}

	durationFromAsset, err := parseDuration(asset.Duration)
	if err != nil {
		return fmt.Errorf("error: %w", err)
	}
	fmt.Println("Dur:", durationFromAsset)

	body, err := app.Immich.DownloadAssets(ctx, asset.ID)
	if err != nil {
		return err
	}
	defer body.Close()

	serverDir := filepath.Join(*app.dirConverted, "tmp/server")
	compressedDir := filepath.Join(*app.dirConverted, "tmp/compressed")
	if err := os.MkdirAll(serverDir, os.ModePerm); err != nil {
		return fmt.Errorf("error creating destination directory: %w", err)
	}
	if err := os.MkdirAll(compressedDir, os.ModePerm); err != nil {
		return fmt.Errorf("error creating destination directory: %w", err)
	}

	tmpOrigFile, err := os.Create(filepath.Join(serverDir, oldFileName))
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %w", err)
	}
	defer tmpOrigFile.Close() // Ensure the temporary file is closed
	tmpOrigPath := tmpOrigFile.Name()
	defer os.Remove(tmpOrigPath) // Ensure the temporary file is closed
	_, err = io.Copy(tmpOrigFile, body)
	if err != nil {
		return fmt.Errorf("failed to write to temporary file: %w", err)
	}
	// fmt.Println("Temp file created %w", tmpOrigPath)

	var out bytes.Buffer
	cmd := exec.Command("exiftool", tmpOrigPath)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("getting exif: %w", err)
	}
	fmt.Println("Captured Output:", out.String())

	exists, err := checkDateTimeOriginal(tmpOrigPath)

	if !exists {
		setDateTimeOriginal(tmpOrigPath, asset.FileCreatedAt.Time)
	}

	tmpNewFile, err := os.Create(filepath.Join(compressedDir, newFileName))
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %w", err)
	}
	defer tmpNewFile.Close() // Ensure the temporary file is closed
	fileNewPath := tmpNewFile.Name()
	defer os.Remove(fileNewPath) // Ensure the temporary file is closed

	// fmt.Printf("Converting: %s | ext: %s\n", asset.OriginalFileName, fileExtension)

	switch asset.Type {
	case "IMAGE":
		cmd := exec.Command("vips", "copy", tmpOrigPath, fileNewPath+"[Q=80]")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("error converting file with vips: %w", err)
		}
	case "VIDEO":
		cmd := exec.Command("HandBrakeCLI", "--preset-import-file", "handbrake.json", "-Z", "immich-upload-optimizer", "-i", tmpOrigPath, "-o", fileNewPath)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("error converting file with HandBrakeCLI: %w", err)
		}
	default:
		fmt.Println("Type is uknown: %w", asset.Type)
	}

	originalSize, err := getFileSize(tmpOrigPath)
	if err != nil {
		return fmt.Errorf("error getting original file size: %w", err)
	}
	convertedSize := originalSize
	if fileNewPath != "" {
		convertedSize, err = getFileSize(fileNewPath)
		if err != nil {
			return fmt.Errorf("error getting converted file size: %w", err)
		}
	}

	originalMB := bytesToMB(originalSize)
	convertedMB := bytesToMB(convertedSize)
	savedMB := bytesToMB(originalSize - convertedSize)

	if (convertedSize + 60000) < originalSize {
		// 1. Get the directory part of the path
		dir := filepath.Dir(fileNewPath)
		// 2. Create an fs.FS representing that directory
		fileSystem := os.DirFS(dir) // Now fileSystem is an fs.FS
		// 3. Get the file name (relative to the directory)
		fileName := filepath.Base(fileNewPath)
		newTitle := strings.TrimSuffix(asset.OriginalFileName, filepath.Ext(asset.OriginalFileName)) + "." + extensionNew
		la := browser.LocalAssetFile{
			FileName: fileName,
			Title:    newTitle,
			FSys:     fileSystem,
		}
		resp, err := app.Immich.AssetReplace(ctx, asset.ID, asset.FileCreatedAt, "immich-go-compress", durationFromAsset, &la)
		if err != nil {
			return fmt.Errorf("send to immich: %w", err)
		}
		fmt.Printf("✓ Replaced (%s): %s (Original: %.2f MB, Converted: %.2f MB, Saved: %.2f MB)\n", resp.Status, asset.OriginalFileName, originalMB, convertedMB, savedMB)
	} else {
		fmt.Printf("✗ Skipped: %s (Original: %.2f MB, Converted: %.2f MB, No size reduction)\n", asset.OriginalFileName, originalMB, convertedMB)
	}
	if err := os.MkdirAll(destDir, os.ModePerm); err != nil {
		return fmt.Errorf("error creating destination directory: %w", err)
	}
	file, err := os.Create(destFile)
	if err != nil {
		return fmt.Errorf("creating file: %w", err)
	}
	defer file.Close()
	return nil
}

func CompressCommand(ctx context.Context, common *cmd.SharedFlags, args []string) error {
	app, err := NewCompressCmd(ctx, common, args)
	if err != nil {
		return err
	}

	fmt.Println("Get server's assets...")
	assets, err := app.Immich.GetAllAssets(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%d received\n", len(assets))

	var wg sync.WaitGroup
	maxGoroutines := 1
	errChan := make(chan error, len(assets)) // Channel to collect errors
	// Use a buffered channel to control concurrency
	semaphore := make(chan struct{}, maxGoroutines)
	for _, asset := range assets {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			wg.Add(1)
			// Acquire a token from the semaphore (blocks if max concurrency is reached)
			semaphore <- struct{}{} // Empty struct{} is the most efficient way to use a channel as a semaphore
			go func() {
				defer wg.Done()

				err := compresFile(ctx, asset, app)
				if err != nil {
					errChan <- err // Send the error to the error channel
				}

				// Release the token (allow another goroutine to run)
				<-semaphore
			}()
		}
	}
	wg.Wait()
	close(errChan) // Close the error channel when all goroutines are done

	// Process the errors collected
	for err := range errChan {
		fmt.Println("Error during convertion:", err)
	}
	return nil
}

func parseDuration(durationStr string) (time.Duration, error) {
	re := regexp.MustCompile(`^(\d+):(\d+):(\d+)\.(\d+)$`) // Modified regex
	matches := re.FindStringSubmatch(durationStr)

	if matches == nil {
		return 0, fmt.Errorf("invalid duration format")
	}

	hours, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0, fmt.Errorf("invalid hours value")
	}

	minutes, err := strconv.Atoi(matches[2])
	if err != nil {
		return 0, fmt.Errorf("invalid minutes value")
	}

	seconds, err := strconv.Atoi(matches[3])
	if err != nil {
		return 0, fmt.Errorf("invalid seconds value")
	}

	milliseconds, err := strconv.Atoi(matches[4])
	if err != nil {
		return 0, fmt.Errorf("invalid milliseconds value")
	}

	duration := time.Duration(hours)*time.Hour +
		time.Duration(minutes)*time.Minute +
		time.Duration(seconds)*time.Second +
		time.Duration(milliseconds)*time.Millisecond

	return duration, nil
}

// Check if DateTimeOriginal exists in metadata
func checkDateTimeOriginal(file string) (bool, error) {
	cmd := exec.Command("exiftool", "-DateTimeOriginal", file)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "Tag 'DateTimeOriginal' not found") {
			return false, nil // Tag not found (no error)
		}
		return false, fmt.Errorf("exiftool error: %v | Output: %s", err, string(output))
	}
	return true, nil // Tag exists
}

// Set DateTimeOriginal to a specific time
func setDateTimeOriginal(file string, newTime time.Time) error {
	timeStr := newTime.Format("2006:01:02 15:04:05")
	cmd := exec.Command(
		"exiftool",
		"-DateTimeOriginal="+timeStr,
		"-overwrite_original", // Avoid creating backup files
		file,
	)
	return cmd.Run()
}
