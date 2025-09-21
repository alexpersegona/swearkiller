package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Segment represents a time range for muting audio
type Segment struct {
	Start float64 // Start time in seconds
	End   float64 // End time in seconds
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
func findSwearTimestamps(srtPath string, swears []string, offset float64) ([]Segment, error) {
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
							fmt.Printf("Warning: Offset %f makes segment (%f, %f) negative, skipping\n", offset, currentStart, currentEnd)
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
					fmt.Printf("Warning: Offset %f makes segment (%f, %f) negative, skipping\n", offset, currentStart, currentEnd)
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

// readSwearsFromFile reads swear words from a text file (one word per line)
func readSwearsFromFile(filePath string) ([]string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open swear file: %v", err)
	}
	defer file.Close()

	var swears []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		swear := strings.TrimSpace(scanner.Text())
		if swear != "" {
			swears = append(swears, swear)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading swear file: %v", err)
	}
	return swears, nil
}

func main() {
	// Command-line flags
	srtFile := flag.String("srt", "", "Path to the SRT subtitle file")
	inputVideo := flag.String("video", "input.mp4", "Path to the input video file")
	outputVideo := flag.String("output", "output.mp4", "Path to the output video file")
	swearFile := flag.String("swears", "", "Path to a file containing swear words (one per line)")
	offset := flag.Float64("offset", 0.0, "Time offset in seconds to adjust SRT timestamps (positive = subtitles too early, negative = subtitles too late)")
	flag.Parse()

	// Validate required flags
	if *srtFile == "" {
		fmt.Println("Error: SRT file path is required (--srt)")
		flag.Usage()
		os.Exit(1)
	}
	if *inputVideo == "" || *outputVideo == "" {
		fmt.Println("Error: Input and output video paths are required (--video, --output)")
		flag.Usage()
		os.Exit(1)
	}

	// Default swear words (if no file provided)
	swears := []string{"asshole", "cunt", "shit", "fuck", "fucker", "mother fucker", "bullshit", "fucking", "shithead", "cock", "jesus", "Jesus", "Christ", "christ", "Jesus Christ", "jesus christ", "Goddammit", "goddammit", "Goddamn", "goddamn", "God damn", "god damn", "bitch", "dickhead"}

	if *swearFile != "" {
		var err error
		swears, err = readSwearsFromFile(*swearFile)
		if err != nil {
			fmt.Printf("Error reading swear file: %v\n", err)
			os.Exit(1)
		}
	}

	// Find timestamps of swears in SRT with offset
	segments, err := findSwearTimestamps(*srtFile, swears, *offset)
	if err != nil {
		fmt.Printf("Error processing SRT file: %v\n", err)
		os.Exit(1)
	}

	// Merge overlapping or close segments
	mergedSegments := mergeSegments(segments)

	// Generate and print FFmpeg command
	ffmpegCmd := generateFFmpegCommand(*inputVideo, *outputVideo, mergedSegments)
	fmt.Println("Generated FFmpeg command:")
	fmt.Println(ffmpegCmd)
}
