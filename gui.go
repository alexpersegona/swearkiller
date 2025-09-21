package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

// Segment represents a time range for muting audio
type Segment struct {
	Start float64 // Start time in seconds
	End   float64 // End time in seconds
}

// SwearKillerApp holds the GUI state
type SwearKillerApp struct {
	srtPath    string
	videoPath  string
	outputPath string
	offset     float64
	swears     []string

	srtLabel        *widget.Label
	videoLabel      *widget.Label
	outputLabel     *widget.Label
	offsetEntry     *widget.Entry
	logText         *widget.Entry
	processBtn      *widget.Button
	executeBtn      *widget.Button
	progressBar     *widget.ProgressBarInfinite
	realProgressBar *widget.ProgressBar
	progressLabel   *widget.Label
	autoOutput      *widget.Check
	settingsBtn     *widget.Button
	lastCommand     string
	myWindow        fyne.Window
}

// parseSRTTime converts SRT timestamp (e.g., "00:01:23,456") to seconds
func parseSRTTime(srtTime string) (float64, error) {
	// Replace comma with period for parsing milliseconds
	srtTime = strings.Replace(srtTime, ",", ".", 1)
	// Parse as duration (HH:MM:SS.sss)
	d, err := time.Parse("15:04:05.000", srtTime)
	if err != nil {
		return 0, fmt.Errorf("failed to parse SRT time %s: %v", srtTime, err)
	}
	// Convert to seconds
	seconds := float64(d.Hour()*3600+d.Minute()*60+d.Second()) + float64(d.Nanosecond())/1e9
	return seconds, nil
}

// findSwearTimestamps searches an SRT file for swear words and returns mute segments
func (app *SwearKillerApp) findSwearTimestamps(srtPath string, swears []string, offset float64) ([]Segment, error) {
	file, err := os.Open(srtPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open SRT file: %v", err)
	}
	defer file.Close()

	var segments []Segment
	var currentStart, currentEnd float64
	var inSubtitleBlock bool
	var subtitleText strings.Builder
	srtTimePattern := regexp.MustCompile(`(\d{2}:\d{2}:\d{2},\d{3})\s*-->\s*(\d{2}:\d{2}:\d{2},\d{3})`)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			// End of a subtitle block
			if inSubtitleBlock {
				// Check for swears in the collected subtitle text
				text := strings.ToLower(subtitleText.String())
				for _, swear := range swears {
					lowerSwear := strings.ToLower(swear)
					if strings.Contains(text, lowerSwear) {
						// Apply offset to timestamps
						adjustedStart := currentStart + offset
						adjustedEnd := currentEnd + offset
						// Ensure timestamps are non-negative
						if adjustedStart < 0 || adjustedEnd < 0 {
							app.log(fmt.Sprintf("Warning: Offset %f makes segment (%f, %f) negative, skipping", offset, currentStart, currentEnd))
							continue
						}
						segments = append(segments, Segment{Start: adjustedStart, End: adjustedEnd})
						break
					}
				}
				inSubtitleBlock = false
				subtitleText.Reset()
			}
			continue
		}
		if srtTimePattern.MatchString(line) && !inSubtitleBlock {
			// Parse timestamp line
			matches := srtTimePattern.FindStringSubmatch(line)
			if len(matches) != 3 {
				continue
			}
			start, err := parseSRTTime(matches[1])
			if err != nil {
				return nil, err
			}
			end, err := parseSRTTime(matches[2])
			if err != nil {
				return nil, err
			}
			currentStart = start
			currentEnd = end
			inSubtitleBlock = true
			continue
		}
		if inSubtitleBlock {
			// Collect subtitle text
			subtitleText.WriteString(line + " ")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading SRT file: %v", err)
	}
	// Process the last subtitle block if it exists
	if inSubtitleBlock {
		text := strings.ToLower(subtitleText.String())
		for _, swear := range swears {
			lowerSwear := strings.ToLower(swear)
			if strings.Contains(text, lowerSwear) {
				// Apply offset to timestamps
				adjustedStart := currentStart + offset
				adjustedEnd := currentEnd + offset
				if adjustedStart >= 0 && adjustedEnd >= 0 {
					segments = append(segments, Segment{Start: adjustedStart, End: adjustedEnd})
				} else {
					app.log(fmt.Sprintf("Warning: Offset %f makes segment (%f, %f) negative, skipping", offset, currentStart, currentEnd))
				}
				break
			}
		}
	}
	return segments, nil
}

// mergeSegments combines overlapping or close segments (within 1 second)
func mergeSegments(segments []Segment) []Segment {
	if len(segments) == 0 {
		return segments
	}
	// Sort segments by start time
	sort.Slice(segments, func(i, j int) bool {
		return segments[i].Start < segments[j].Start
	})

	var merged []Segment
	current := segments[0]
	for i := 1; i < len(segments); i++ {
		if segments[i].Start <= current.End+1.0 {
			// Merge if segments overlap or are within 1 second
			if segments[i].End > current.End {
				current.End = segments[i].End
			}
		} else {
			merged = append(merged, current)
			current = segments[i]
		}
	}
	merged = append(merged, current)
	return merged
}

// generateFFmpegCommand creates an FFmpeg command to mute audio for the given segments
func generateFFmpegCommand(inputVideo, outputVideo string, segments []Segment) string {
	if len(segments) == 0 {
		return fmt.Sprintf("No segments to mute. Copying input to output: ffmpeg -i %q -c copy %q", inputVideo, outputVideo)
	}

	var enableConditions []string
	for _, seg := range segments {
		enableConditions = append(enableConditions, fmt.Sprintf("between(t,%.3f,%.3f)", seg.Start, seg.End))
	}
	// Combine conditions with '+' for a single volume filter
	enableExpr := strings.Join(enableConditions, "+")
	filter := fmt.Sprintf("volume=enable='%s':volume=0", enableExpr)

	return fmt.Sprintf("ffmpeg -i %q -af %q -c:v copy -c:a aac %q", inputVideo, filter, outputVideo)
}

// log adds a message to the log text area
func (app *SwearKillerApp) log(message string) {
	if app.logText == nil {
		fmt.Printf("LOG: %s\n", message) // Fallback to console if UI not ready
		return
	}
	current := app.logText.Text
	if current != "" {
		current += "\n"
	}
	app.logText.SetText(current + message)

	// Auto-scroll to bottom to show latest content
	app.logText.CursorRow = len(strings.Split(app.logText.Text, "\n"))
}

// clearLog clears the log text area
func (app *SwearKillerApp) clearLog() {
	app.logText.SetText("")
}

// updateProcessButton enables/disables the process button based on required inputs
func (app *SwearKillerApp) updateProcessButton() {
	// Safety check for nil pointers
	if app.processBtn == nil || app.executeBtn == nil {
		return
	}

	var canProcess bool
	if app.autoOutput != nil && app.autoOutput.Checked {
		// If auto output is enabled, only need SRT and video files
		canProcess = app.srtPath != "" && app.videoPath != ""
		if canProcess {
			// Auto-generate output path
			app.generateAutoOutputPath()
		}
	} else {
		// Manual output selection required
		canProcess = app.srtPath != "" && app.videoPath != "" && app.outputPath != ""
	}

	if canProcess {
		app.processBtn.Enable()
	} else {
		app.processBtn.Disable()
	}

	// Enable execute button only if we have a command
	if app.lastCommand != "" && canProcess {
		app.executeBtn.Enable()
	} else {
		app.executeBtn.Disable()
	}
}

// generateAutoOutputPath creates output path based on input video with "-CLEAN" suffix
func (app *SwearKillerApp) generateAutoOutputPath() {
	if app.videoPath == "" || app.outputLabel == nil {
		return
	}

	// Get directory and filename without extension
	dir := filepath.Dir(app.videoPath)
	filename := filepath.Base(app.videoPath)
	ext := filepath.Ext(filename)
	nameWithoutExt := strings.TrimSuffix(filename, ext)

	// Create new filename with -CLEAN suffix and .mp4 extension
	cleanFilename := nameWithoutExt + "-CLEAN.mp4"
	app.outputPath = filepath.Join(dir, cleanFilename)

	// Update the label
	app.outputLabel.SetText(fmt.Sprintf("Output: %s", cleanFilename))
}

// processVideo runs the swear killing process
func (app *SwearKillerApp) processVideo() {
	app.clearLog()
	app.log("Starting swear killer process...")

	// Parse offset
	offsetStr := app.offsetEntry.Text
	if offsetStr == "" {
		app.offset = 0.0
	} else {
		var err error
		app.offset, err = strconv.ParseFloat(offsetStr, 64)
		if err != nil {
			app.log(fmt.Sprintf("Error: Invalid offset value: %v", err))
			return
		}
	}

	app.log(fmt.Sprintf("Using offset: %.1f seconds", app.offset))
	app.log(fmt.Sprintf("Processing SRT: %s", app.srtPath))
	app.log(fmt.Sprintf("Input video: %s", app.videoPath))
	app.log(fmt.Sprintf("Output video: %s", app.outputPath))

	// Find swear timestamps
	segments, err := app.findSwearTimestamps(app.srtPath, app.swears, app.offset)
	if err != nil {
		app.log(fmt.Sprintf("Error processing SRT file: %v", err))
		return
	}

	app.log(fmt.Sprintf("Found %d swear segments", len(segments)))

	// Merge overlapping segments
	mergedSegments := mergeSegments(segments)
	app.log(fmt.Sprintf("Merged to %d segments", len(mergedSegments)))

	// Generate FFmpeg command
	ffmpegCmd := generateFFmpegCommand(app.videoPath, app.outputPath, mergedSegments)
	app.lastCommand = ffmpegCmd
	app.log("\n=== GENERATED FFMPEG COMMAND ===")
	if ffmpegCmd == "" {
		app.log("ERROR: Generated command is empty!")
	} else {
		app.log(ffmpegCmd)
	}
	app.log("=====================================")
	app.log("\nClick 'Execute FFmpeg' to run the command automatically!")
	app.updateProcessButton()
}

// executeFFmpeg runs the generated FFmpeg command
func (app *SwearKillerApp) executeFFmpeg() {
	// Add safety checks
	if app.progressBar == nil || app.processBtn == nil || app.executeBtn == nil {
		app.log("Error: UI components not initialized")
		return
	}

	if app.lastCommand == "" {
		app.log("Error: No FFmpeg command to execute")
		return
	}

	app.log("\n=== Executing FFmpeg Command ===")
	app.log("Starting video processing...")

	app.progressBar.Show()

	// Disable buttons during execution
	app.processBtn.Disable()
	app.executeBtn.Disable()

	// Check if input files exist first
	if _, err := os.Stat(app.videoPath); os.IsNotExist(err) {
		app.log(fmt.Sprintf("Error: Input video file does not exist: %s", app.videoPath))
		return
	}

	// Check if output directory exists and is writable
	outputDir := filepath.Dir(app.outputPath)
	if _, err := os.Stat(outputDir); os.IsNotExist(err) {
		app.log(fmt.Sprintf("Error: Output directory does not exist: %s", outputDir))
		return
	}

	// Get volume filter safely
	volumeFilter := app.getVolumeFilter()
	if volumeFilter == "" {
		app.log("Error: Could not generate volume filter")
		return
	}

	// Build FFmpeg command with proper arguments
	args := []string{
		"-i", app.videoPath,
		"-af", fmt.Sprintf("volume=enable='%s':volume=0", volumeFilter),
		"-c:v", "copy",
		"-c:a", "aac",
		"-y", // Overwrite output file if it exists
		app.outputPath,
	}

	app.log(fmt.Sprintf("Running: ffmpeg %s", strings.Join(args, " ")))

	// Get video duration for progress calculation
	duration, err := app.getVideoDuration()
	if err != nil {
		app.log(fmt.Sprintf("Warning: Could not get video duration: %v", err))
		duration = 0 // Fall back to spinner
	}

	if duration > 0 {
		app.log(fmt.Sprintf("📹 Video duration: %.1f minutes", duration/60))
		app.progressBar.Hide()
		app.realProgressBar.Show()
		app.progressLabel.Show()
		app.progressLabel.SetText("Preparing to process video...")
	} else {
		app.log("⏳ Processing video... This may take several minutes depending on video length.")
	}

	// Run ffmpeg command in a separate goroutine to keep UI responsive
	go func() {
		defer func() {
			if r := recover(); r != nil {
				app.log(fmt.Sprintf("Panic during FFmpeg execution: %v", r))
			}
			if app.progressBar != nil {
				app.progressBar.Hide()
			}
			if app.realProgressBar != nil {
				app.realProgressBar.Hide()
			}
			if app.progressLabel != nil {
				app.progressLabel.Hide()
			}
			app.enableButtons()
		}()

		// Add progress flag to FFmpeg - use stdout for progress
		progressArgs := make([]string, 0, len(args)+2)
		progressArgs = append(progressArgs, args[:len(args)-1]...)
		progressArgs = append(progressArgs, "-progress", "pipe:1")
		progressArgs = append(progressArgs, args[len(args)-1])
		cmd := exec.Command("ffmpeg", progressArgs...)

		// Set up pipes to capture stdout for progress
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			app.log(fmt.Sprintf("Error setting up progress pipe: %v", err))
			return
		}

		// Start the command
		if err := cmd.Start(); err != nil {
			app.log(fmt.Sprintf("Error starting FFmpeg: %v", err))
			return
		}

		// Read progress in real-time if we have duration
		if duration > 0 {
			go func() {
				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					line := scanner.Text()

					// Only log relevant progress lines to keep output clean
					if strings.Contains(line, "time=") || strings.Contains(line, "out_time_us=") || strings.Contains(line, "progress=") {
						fyne.Do(func() {
							app.log(fmt.Sprintf("FFmpeg: %s", line))
						})
					}

					currentTime, found := parseFFmpegProgress(line)
					if found {
						percentage := (currentTime / duration) * 100
						if percentage > 100 {
							percentage = 100
						}

						// Fyne ProgressBar expects values between 0.0 and 1.0, not 0-100
						progressValue := percentage / 100.0
						if progressValue > 1.0 {
							progressValue = 1.0
						}

						remainingTime := duration - currentTime
						if remainingTime < 0 {
							remainingTime = 0
						}

						// Log progress occasionally to show it's working
						if int(currentTime)%30 == 0 || percentage >= 99 {
							fyne.Do(func() {
								app.log(fmt.Sprintf("⏳ Progress: %.1f%% complete", percentage))
							})
						}

						// Update UI elements on the main thread
						fyne.Do(func() {
							if app.realProgressBar != nil {
								app.realProgressBar.SetValue(progressValue)
							}
							if app.progressLabel != nil {
								app.progressLabel.SetText(fmt.Sprintf("Processing: %.1f%% complete (%.1fs remaining)",
									percentage, remainingTime))
							}
						})
					}
				}
			}()
		}

		// Wait for command to complete
		err = cmd.Wait()

		if err != nil {
			fyne.Do(func() {
				app.log(fmt.Sprintf("❌ Error executing FFmpeg: %v", err))
			})
		} else {
			fyne.Do(func() {
				if app.realProgressBar != nil {
					app.realProgressBar.SetValue(1.0) // 1.0 = 100% for Fyne
				}
				if app.progressLabel != nil {
					app.progressLabel.SetText("✅ Processing complete!")
				}
				app.log("✅ Video processing completed successfully!")
				app.log(fmt.Sprintf("📁 Clean video saved to: %s", app.outputPath))
				app.log("🎉 You can now play your clean video!")
			})
		}
	}()
}

// getVolumeFilter extracts just the volume filter part from the last command
func (app *SwearKillerApp) getVolumeFilter() string {
	// Extract the volume filter from the generated command
	cmdStr := app.lastCommand
	start := strings.Index(cmdStr, "between(")
	end := strings.LastIndex(cmdStr, ")")
	if start == -1 || end == -1 {
		return ""
	}
	return cmdStr[start : end+1]
}

// enableButtons re-enables the buttons after execution
func (app *SwearKillerApp) enableButtons() {
	app.updateProcessButton()
}

// getVideoDuration gets the total duration of the video in seconds
func (app *SwearKillerApp) getVideoDuration() (float64, error) {
	cmd := exec.Command("ffprobe", "-v", "quiet", "-show_entries", "format=duration", "-of", "csv=p=0", app.videoPath)
	output, err := cmd.Output()
	if err != nil {
		return 0, err
	}

	durationStr := strings.TrimSpace(string(output))
	duration, err := strconv.ParseFloat(durationStr, 64)
	if err != nil {
		return 0, err
	}

	return duration, nil
}

// parseFFmpegProgress parses FFmpeg progress output and returns current time in seconds
func parseFFmpegProgress(line string) (float64, bool) {
	// Look for "out_time_us=" (microseconds)
	if strings.Contains(line, "out_time_us=") {
		timeRegex := regexp.MustCompile(`out_time_us=(\d+)`)
		matches := timeRegex.FindStringSubmatch(line)
		if len(matches) == 2 {
			microseconds, err := strconv.ParseInt(matches[1], 10, 64)
			if err == nil {
				seconds := float64(microseconds) / 1000000.0
				return seconds, true
			}
		}
	}

	// Skip out_time_ms= - FFmpeg puts microseconds there, not milliseconds!

	// Skip out_time= - regex not working properly for HH:MM:SS format

	// Skip time= format too - not needed since out_time_us= works perfectly
	return 0, false
}

// Settings structure for saving/loading configuration
type Settings struct {
	SwearWords []string `json:"swear_words"`
}

// getSettingsPath returns the path to the settings file
func getSettingsPath() string {
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".swear-killer-settings.json")
}

// loadSettings loads swear words from settings file
func (app *SwearKillerApp) loadSettings() {
	settingsPath := getSettingsPath()
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		// Use default swear words if no settings file exists
		return
	}

	var settings Settings
	if err := json.Unmarshal(data, &settings); err != nil {
		return
	}

	if len(settings.SwearWords) > 0 {
		app.swears = settings.SwearWords
	}
}

// saveSettings saves current swear words to settings file
func (app *SwearKillerApp) saveSettings() error {
	settings := Settings{
		SwearWords: app.swears,
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}

	settingsPath := getSettingsPath()
	return os.WriteFile(settingsPath, data, 0644)
}

// showSettings displays the settings dialog
func (app *SwearKillerApp) showSettings() {
	// Create a large text area for editing swear words
	swearText := widget.NewMultiLineEntry()
	swearText.SetText(strings.Join(app.swears, "\n"))
	swearText.Resize(fyne.NewSize(400, 300))

	// Instructions label
	instructions := widget.NewLabel("Edit swear words (one per line):")

	// Scroll container for the text area
	scroll := container.NewScroll(swearText)
	scroll.SetMinSize(fyne.NewSize(400, 300))

	// Buttons
	saveBtn := widget.NewButton("Save", func() {
		// Parse the text and update swear words
		text := strings.TrimSpace(swearText.Text)
		if text == "" {
			app.swears = []string{}
		} else {
			lines := strings.Split(text, "\n")
			app.swears = []string{}
			for _, line := range lines {
				word := strings.TrimSpace(line)
				if word != "" {
					app.swears = append(app.swears, word)
				}
			}
		}

		// Save to file
		if err := app.saveSettings(); err != nil {
			dialog.ShowError(err, app.myWindow)
		} else {
			dialog.ShowInformation("Settings Saved",
				fmt.Sprintf("Saved %d swear words to settings", len(app.swears)),
				app.myWindow)
		}
	})

	resetBtn := widget.NewButton("Reset to Defaults", func() {
		// Reset to default swear words
		app.swears = []string{"asshole", "cunt", "shit", "fuck", "fucker", "mother fucker", "bullshit", "fucking", "shithead", "cock", "jesus", "christ", "jesus christ", "goddammit", "goddamn", "god damn", "bitch", "dickhead"}
		swearText.SetText(strings.Join(app.swears, "\n"))
	})

	cancelBtn := widget.NewButton("Cancel", func() {
		// Just close the dialog - no changes
	})

	buttonContainer := container.NewHBox(saveBtn, resetBtn, cancelBtn)

	content := container.NewVBox(
		instructions,
		scroll,
		buttonContainer,
	)

	// Create and show dialog
	settingsDialog := dialog.NewCustom("Swear Words Settings", "Close", content, app.myWindow)
	settingsDialog.Resize(fyne.NewSize(500, 450))
	settingsDialog.Show()
}

func main() {
	myApp := app.NewWithID("com.swear-killer.app")
	myApp.SetIcon(nil) // You can add an icon later

	myWindow := myApp.NewWindow("Swear Killer")
	myWindow.Resize(fyne.NewSize(700, 750)) // Make window narrower but taller

	// Initialize app state
	swearApp := &SwearKillerApp{
		// Default swear words
		swears:   []string{"asshole", "cunt", "shit", "fuck", "fucker", "mother fucker", "bullshit", "fucking", "shithead", "cock", "jesus", "christ", "jesus christ", "goddammit", "goddamn", "god damn", "bitch", "dickhead"},
		myWindow: myWindow,
	}

	// Load saved settings (will override defaults if settings file exists)
	swearApp.loadSettings()

	// Create UI elements
	title := widget.NewLabelWithStyle("Swear Killer", fyne.TextAlignCenter, fyne.TextStyle{Bold: true})

	// SRT file selection
	swearApp.srtLabel = widget.NewLabel("No SRT file selected")
	srtButton := widget.NewButton("Select SRT File", func() {
		dialog.ShowFileOpen(func(reader fyne.URIReadCloser, err error) {
			if err != nil || reader == nil {
				return
			}
			defer reader.Close()
			swearApp.srtPath = reader.URI().Path()
			swearApp.srtLabel.SetText(fmt.Sprintf("SRT: %s", reader.URI().Name()))
			swearApp.updateProcessButton()
		}, myWindow)
	})

	// Video file selection
	swearApp.videoLabel = widget.NewLabel("No video file selected")
	videoButton := widget.NewButton("Select Video File", func() {
		dialog.ShowFileOpen(func(reader fyne.URIReadCloser, err error) {
			if err != nil || reader == nil {
				return
			}
			defer reader.Close()
			swearApp.videoPath = reader.URI().Path()
			swearApp.videoLabel.SetText(fmt.Sprintf("Video: %s", reader.URI().Name()))
			swearApp.updateProcessButton()
		}, myWindow)
	})

	// Output file selection
	swearApp.outputLabel = widget.NewLabel("Output will be auto-generated")
	outputButton := widget.NewButton("Select Output Location", func() {
		dialog.ShowFileSave(func(writer fyne.URIWriteCloser, err error) {
			if err != nil || writer == nil {
				return
			}
			defer writer.Close()
			outputPath := writer.URI().Path()

			// Ensure the output file has a proper extension
			validExtensions := []string{".mp4", ".mkv", ".avi", ".mov", ".webm", ".flv", ".wmv", ".m4v", ".3gp"}
			hasValidExtension := false
			lowerPath := strings.ToLower(outputPath)

			for _, ext := range validExtensions {
				if strings.HasSuffix(lowerPath, ext) {
					hasValidExtension = true
					break
				}
			}

			if !hasValidExtension {
				outputPath += ".mp4" // Default to MP4 if no valid extension
			}

			swearApp.outputPath = outputPath
			swearApp.outputLabel.SetText(fmt.Sprintf("Output: %s", writer.URI().Name()))
			swearApp.updateProcessButton()
		}, myWindow)
	})

	// Auto output checkbox (defined after outputButton)
	swearApp.autoOutput = widget.NewCheck("Auto-generate output filename (adds '-CLEAN.mp4')", func(checked bool) {
		if checked {
			outputButton.Disable()
			swearApp.outputLabel.SetText("Output will be auto-generated")
		} else {
			outputButton.Enable()
			swearApp.outputLabel.SetText("No output file selected")
			swearApp.outputPath = ""
		}
		swearApp.updateProcessButton()
	})
	swearApp.autoOutput.SetChecked(true) // Default to auto-generate
	outputButton.Disable()               // Start disabled since auto-generate is default

	// Offset control
	offsetLabel := widget.NewLabel("Time Offset (seconds):")
	swearApp.offsetEntry = widget.NewEntry()
	swearApp.offsetEntry.SetPlaceHolder("0.0 (negative = earlier, positive = later)")

	// Process button
	swearApp.processBtn = widget.NewButton("Generate FFmpeg Command", swearApp.processVideo)
	swearApp.processBtn.Disable()

	// Execute button
	swearApp.executeBtn = widget.NewButton("Execute FFmpeg", swearApp.executeFFmpeg)
	swearApp.executeBtn.Disable()

	// Settings button
	swearApp.settingsBtn = widget.NewButton("Settings", swearApp.showSettings)

	// Progress bars
	swearApp.progressBar = widget.NewProgressBarInfinite()
	swearApp.progressBar.Hide()

	swearApp.realProgressBar = widget.NewProgressBar()
	swearApp.realProgressBar.SetValue(0) // Initialize to 0%
	swearApp.realProgressBar.Hide()

	swearApp.progressLabel = widget.NewLabel("")
	swearApp.progressLabel.Hide()

	// Log text area
	swearApp.logText = widget.NewMultiLineEntry()
	swearApp.logText.SetPlaceHolder("Process log will appear here...")
	swearApp.logText.Wrapping = fyne.TextWrapWord // Enable word wrapping to prevent horizontal scroll
	logScroll := container.NewScroll(swearApp.logText)
	logScroll.SetMinSize(fyne.NewSize(500, 400)) // Narrower width, taller height

	// Layout
	fileSection := container.NewVBox(
		srtButton, swearApp.srtLabel,
		videoButton, swearApp.videoLabel,
		swearApp.autoOutput,
		outputButton, swearApp.outputLabel,
	)

	offsetSection := container.NewVBox(
		offsetLabel,
		swearApp.offsetEntry,
	)

	buttonSection := container.NewHBox(
		swearApp.processBtn,
		swearApp.executeBtn,
		swearApp.settingsBtn,
	)

	progressSection := container.NewVBox(
		swearApp.progressBar,
		swearApp.realProgressBar,
		swearApp.progressLabel,
	)

	content := container.NewVBox(
		title,
		widget.NewSeparator(),
		fileSection,
		widget.NewSeparator(),
		offsetSection,
		widget.NewSeparator(),
		buttonSection,
		progressSection,
		widget.NewSeparator(),
		widget.NewLabel("Output Log:"),
		logScroll,
	)

	myWindow.SetContent(container.NewPadded(content))
	myWindow.ShowAndRun()
}
