# resmon

Finds the resource bottleneck (RAM, CPU, disk, GPU, heat) behind a laggy Mac and the apps causing it.

```sh
make                  # build ./resmon (or: go build -o resmon ./cmd/resmon)
make install          # copy to ~/.local/bin (PREFIX=... or BINDIR=... to change)
go install github.com/duluk/resource-monitor-mac/cmd/resmon@latest   # or install straight from GitHub
./resmon              # sample 15s, print the measurements, then a diagnosis
sudo ./resmon         # same, with full detail for system processes (WindowServer, kernel_task, daemons)
./resmon -watch       # one line every 5s until Ctrl-C; flags swapping, CPU/GPU saturation, throttling, lag
./resmon -d 60s -n 20 # longer sample, bigger tables
./resmon -json        # machine-readable report (includes the diagnosis)
./resmon -ai          # also ask an OpenAI model to analyze the report
./resmon -ask "typing lags when I switch apps"   # same, with your symptoms as context
```

Other make targets: `make run ARGS="-d 30s"`, `make run-root` (with sudo), `make watch`, `make check` (gofmt, vet and tests), `make clean`, `make help`.

The report has two parts:

1. **Measurements:** memory breakdown and paging rates, CPU, GPU, disk, thermal state, latency, and the top apps and processes.
2. **Diagnosis:** a verdict naming the bottleneck, then a plain-English explanation of what's going on and who is responsible. It ends with findings ranked by severity, what looks fine (so you don't chase it), and a numbered list of what to do.

## AI analysis (`-ai`)

`-ai` sends the plain-text report to OpenAI's Responses API and prints the model's analysis after the built-in diagnosis. The report includes app and process names. It needs `OPENAI_API_KEY`.

- Model: `-model`, or `OPENAI_MODEL`. Default `gpt-6.1-terra`.
- Endpoint: `OPENAI_BASE_URL`. Default `https://api.openai.com/v1`. Any compatible server that implements `/responses` works.
- `-ask "..."` adds your own description of the problem, or a question, to the prompt. It implies `-ai`.

What it looks at:

- **Memory:** pressure level, swap size and live swap traffic, compressor churn, file page-ins, an estimate of how much memory the workload wants versus installed RAM, and per-app physical footprint. Footprint includes compressed and swapped memory, so it matches Activity Monitor.
- **CPU:** total and kernel time, load average, multi-core apps, processes pinned at 100%, WindowServer and kernel_task, and known background work (Spotlight, Time Machine, iCloud, updates, antivirus/EDR agents).
- **Disk:** free space (swap lives on the startup disk), throughput, and average I/O latency.
- **GPU** utilization, **thermal state**, and Low Power Mode.
- **Latency probe:** a thread at UI priority repeatedly sleeps and measures how late it wakes up. This shows CPU scheduling delay, the kind that delays keystroke handling.

Helper processes are grouped under the app responsible for them. Browser tab processes count toward their browser, and commands run in a terminal count toward that terminal. The "Largest process" column shows which member process is actually responsible.
