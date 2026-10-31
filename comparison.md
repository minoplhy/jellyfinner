# Encoding comparison

Measurements and estimates from one deployment, not general figures.

## Estimated encode time on the current worker

The worker is 1 vCPU on a Ryzen 9 9950X, encoding with x264. The times below cover encoding only, not the pull, verification or push.

Only the "Synthetic" rows come from a measurement: a 60-second 1080p test pattern with stock ffmpeg and `-tune animation`. That run has no decoding and no real footage, so treat it as a best case. The other rows are guesses, to be replaced by the real episode benchmark in Phase 0 step 3.

| Source and preset | Speed | Basis | 24-minute episode | 2-hour film |
|---|---|---|---|---|
| Synthetic 1080p, `veryfast` | 2.37x | Measured | 10 min | 51 min |
| Synthetic 1080p, `medium` | 1.06x | Measured | 23 min | 1 h 53 min |
| Real 1080p, `veryfast` | 1x to 2x | Guess | 12 to 24 min | 1 to 2 h |
| Real 1080p, `medium` | 0.4x to 0.8x | Guess | 30 to 60 min | 2.5 to 5 h |
| 4K HDR tonemapped to 1080p | 0.1x to 0.25x | Guess | 1.5 to 4 h | 8 to 20 h |

Compliant H.264 files are stream-copied, so they skip encoding and should take a few minutes at most.

## Episodes per hour of CPU burst

This worker can run at full CPU for about an hour before the provider throttles it. Speed after that is not measured.

| Preset | Synthetic speed | Guessed real speed |
|---|---|---|
| `veryfast` | About 6 | 2.5 to 5 |
| `medium` | About 2.5 | 1 to 2 |
