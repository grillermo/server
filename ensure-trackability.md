# top_cpu Service Trackability

Services launched by `/Users/grillermo/c/server/after-reboot.sh` must keep a meaningful process title for every long-running child. Use the format:

```text
service-role
```

The launcher runs each service through `/Users/grillermo/c/server/top-cpu-service-wrapper`, which exports:

```bash
TOP_CPU_SERVICE
TOP_CPU_SERVICE_ROOT
TOP_CPU_SERVICE_TMUX
TOP_CPU_ROLE
```

The wrapper also writes `~/.local/share/top_cpu/services/<service>.env`, allowing `top_cpu` to attribute descendants by wrapper ancestry.

## Ruby / Rack / Rails / Puma

Set titles in boot hooks, Puma config, Rack startup, and job entrypoints when the default title would be `ruby`, `rackup`, `rails`, or `puma`:

```ruby
Process.setproctitle("readitsoon-puma")
Process.setproctitle("readitsoon-jobs")
```

## Python / FastAPI / Uvicorn

Use `setproctitle`, including startup/lifespan paths so reload children retitle themselves:

```python
from setproctitle import setproctitle

setproctitle("patatatube-api")
```

## Node / Bun

Prefer the runtime title API:

```js
process.title = "blog_grillermo_com-web";
```

Where the runtime overwrites titles, launch the process with `exec -a service-role`.

## PHP / Kimai

Prefer wrapper-level aliases for CLI server entrypoints:

```bash
exec -a kimai-web php -S localhost:8001 -t public/
```

Use `cli_set_process_title("kimai-web")` only for PHP entrypoints where it is known to work.
