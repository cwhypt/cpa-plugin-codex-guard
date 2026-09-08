# Windows 本机 CPA 升级与重启流程

> ⚠️ **仅适用于 Windows 本机**（`C:\Users\cwhyp\Documents\GitHub\CLIProxyAPI`）。
> Linux 服务器侧走 systemd 用户单元（`cliproxyapi.service`），见 `cliproxyapi.service` 与 `post-401-after-upgrade.md`，不要混用。

## 核心原则（Windows 必看）

1. **启动必须显式指定 `-WorkingDirectory`**
   - 插件目录（`plugins/`）、`config.yaml`、`.env`、插件状态文件（如 `data/cpa-codex-guard-state.json`）均按**进程工作目录（CWD）**解析。
   - `Start-Process` 漏传 `-WorkingDirectory` 时，CWD 退化为执行命令时的终端目录，表现为插件加载异常、状态文件写错地方/静默丢失。
   - 前车之鉴见 `cpa-plugin-codex-guard` 仓 `docs/postmortem-probe-cache-persistence-and-crash.md`。
2. **升级只替换主程序二进制**，不动 `config.yaml`、`.env`、`auths/`、`plugins/`。
3. **官方 Release 包内二进制叫 `cli-proxy-api.exe`**，本地习惯重命名为 `CLIProxyAPI.exe` 运行（Windows 文件名不区分大小写，实为同一文件）。
4. **真实版本以二进制输出为准**：`version.txt` / 目录名里的版本号可能滞后（同 Linux 侧惯例）。

## 一、升级流程（已验证：7.2.152，2026-09-08）

上游仓库为 `router-for-me/CLIProxyAPI`（注意不是 fork），预编译资产为 `CLIProxyAPI_<ver>_windows_amd64.zip`。

### 1. 停止旧进程（先停，否则文件锁占住换不掉）

```powershell
Get-Process -Name "CLIProxyAPI" -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Seconds 2
Get-Process -Name "CLIProxyAPI" -ErrorAction SilentlyContinue
```

### 2. 下载 Release 并校验 SHA256

```powershell
$ver = "7.2.152"
$tmp = "$env:LOCALAPPDATA\Temp\cpa-update"
if (-not (Test-Path $tmp)) { New-Item -ItemType Directory -Path $tmp -Force | Out-Null }

$zip = "$tmp\CLIProxyAPI_${ver}_windows_amd64.zip"
Invoke-WebRequest -Uri "https://github.com/router-for-me/CLIProxyAPI/releases/download/v${ver}/CLIProxyAPI_${ver}_windows_amd64.zip" -OutFile $zip

# 与 Release 页 checksums.txt 对照（7.2.152 实测：7b01cc85…f764ce）
(Get-FileHash -Algorithm SHA256 $zip).Hash.ToLower()
```

### 3. 备份旧版 → 解压替换

```powershell
$repo = "C:\Users\cwhyp\Documents\GitHub\CLIProxyAPI"
Copy-Item "$repo\CLIProxyAPI.exe" "$repo\CLIProxyAPI.exe.bak" -Force

$ext = "$tmp\extracted"
if (Test-Path $ext) { Remove-Item $ext -Recurse -Force }
Expand-Archive -Path $zip -DestinationPath $ext -Force
Copy-Item "$ext\cli-proxy-api.exe" "$repo\CLIProxyAPI.exe" -Force
```

### 4. 按“重启流程”拉起并验证（见下）

## 二、重启流程

### 方式 1：后台常驻（推荐，一键脚本见 `scripts/restart-cpa-windows.ps1`）

```powershell
$repo = "C:\Users\cwhyp\Documents\GitHub\CLIProxyAPI"
Get-Process -Name "CLIProxyAPI" -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Seconds 2

Start-Process -FilePath "$repo\CLIProxyAPI.exe" `
    -ArgumentList "--config","config.yaml" `
    -WorkingDirectory $repo `
    -WindowStyle Hidden
Start-Sleep -Seconds 3

Get-Process -Name "CLIProxyAPI" -ErrorAction SilentlyContinue | Select-Object Id, StartTime
```

### 方式 2：前台调试（看实时日志 / 跑 `--antigravity-login` 走 OAuth 时用）

```powershell
Set-Location "C:\Users\cwhyp\Documents\GitHub\CLIProxyAPI"
.\CLIProxyAPI.exe --config config.yaml
```

## 三、升级/重启后验证清单

1. **主日志**（确认插件注册 + 账号加载）：
   ```powershell
   Get-Content "$env:USERPROFILE\.cli-proxy-api\logs\main.log" -Tail 30
   ```
   - 找 `pluginhost: plugin registered`（插件正常注册）
   - 找 `full client load complete` / 版本行 `v7.2.152`（账号与配置载入）
2. **接口健康检查**（200 + 模型列表即就绪）：
   ```powershell
   curl.exe -s http://127.0.0.1:8317/v1/models -H "Authorization: Bearer <CPA-API-Key>"
   ```
3. **Antigravity 账号文件**位置（Windows 本机）：`$env:USERPROFILE\.cli-proxy-api\antigravity-<邮箱>.json`。
   若转发报 403 `verify your account to continue`，是 Google 上游对该账号的限制（非 CPA 问题）：
   用同账号浏览器打开 `https://cloudcode.google.com` 按提示完成验证，或换号重跑 `.\CLIProxyAPI.exe --antigravity-login`。

## 四、与 Linux 侧的差异对照

| 项 | Windows 本机（本文） | Linux 服务器 |
|---|---|---|
| 启动方式 | `Start-Process … -WorkingDirectory` | `systemctl --user restart cliproxyapi` |
| 二进制名 | `CLIProxyAPI.exe`（Release 包内叫 `cli-proxy-api.exe`） | `cli-proxy-api` |
| 升级 | 手动下载 zip 替换 | 二进制原地替换后重启 |
| 401 抖动 | 未观察到 | 见 `post-401-after-upgrade.md`（热重载抖动，先隔 1–2 分钟重测） |
