# How To Rip Swears Out

## Compile any GO updates to the script if needed
```bash
go build -o ./swear-killer
```

## Examples of How to Run Swear Killer
The following uses **nobody.2** as an example file name. Update the paths and filenames to match your use case.
```bash
./swear-killer \
  --srt "/Users/alex/Downloads/nobody.2/nobody.2.srt" \
  --video "/Users/alex/Downloads/nobody.2/nobody.2.mkv" \
  --output "/Users/alex/Downloads/nobody.2/nobody.2-clean.mp4" \
  --offset -0.5
```

## Extract embedded subtitles (if needed)
Some videos have embedded srt files. To extract them, run the command below. If there are multiple subtitle streams, you might need to try different indices:

```bash
# Check what subtitle streams are available
ffmpeg -i "/Users/alex/Downloads/nobody.2/nobody.2.mkv" 2>&1 | grep -i subtitle

# Extract the first subtitle stream
ffmpeg -i "/Users/alex/Downloads/nobody.2/nobody.2.mkv" -map 0:s:0 -c:s srt extracted-subtitles.srt
```
```
body.2.mkv" -map 0:s:0 -c:s srt extracted-subtitles.srt
```
