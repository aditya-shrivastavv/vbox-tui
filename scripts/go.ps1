param([Parameter(ValueFromRemainingArguments = $true)][string[]]$GoArgs)

# Find Go toolchain on PATH, falling back to standard Windows install directory if needed.
$goCmd = Get-Command go -ErrorAction SilentlyContinue
if ($goCmd) {
    $goExe = $goCmd.Source
} elseif (Test-Path "C:\Program Files\Go\bin\go.exe") {
    $goExe = "C:\Program Files\Go\bin\go.exe"
} else {
    Write-Error "Go executable not found on PATH or in C:\Program Files\Go\bin\go.exe"
    exit 1
}

& $goExe @GoArgs
exit $LASTEXITCODE
