# One reader for the `pricing:` block of an InferGate config, shared by the two
# M4 scripts.
#
# WHY IT EXISTS. Both scripts derive money from list prices, and neither can
# carry a copy of them: a price edited in configs\tiered-local.yaml would
# otherwise leave a gate green while its arithmetic priced a config nobody runs.
# The reader lives here so scripts\measure-m4.ps1 (which produces the recorded
# numbers) and scripts\verify-m4.ps1 (which re-runs the claim against real
# binaries) cannot disagree about how a price is read out of the config.
#
# WHAT IT READS. internal/gateway/pricing.go prices a request by the MODEL the
# response carries, so the entries that matter are `pricing.models.<alias>` for
# the models a request can name and `pricing.default` for everything else. Both
# the inline flow form (`default: {in: 1.0, out: 3.0}`) and the nested block form
# configs\tiered-local.yaml uses (`default:` followed by indented `in:`/`out:`)
# are handled. A missing `pricing:` block yields a $null Default and an empty
# Models rather than an error: every caller asserts on what it needs before it
# divides by anything.

function Get-PricingFromConfig {
    param([string]$Text)
    $lines = $Text -split "`r?`n"
    $inPricing = $false
    $inModels = $false
    $default = $null
    $models = [ordered]@{}
    $pending = ''
    foreach ($line in $lines) {
        $clean = ($line -replace '#.*$', '')
        if ($clean.Trim().Length -eq 0) { continue }
        $indent = $clean.Length - $clean.TrimStart().Length
        $t = $clean.Trim()
        if ($t -match '^pricing:\s*$') { $inPricing = $true; $inModels = $false; continue }
        if ($inPricing) {
            if ($indent -eq 0) { $inPricing = $false; $inModels = $false; continue }
            if ($t -match '^default:\s*\{\s*in:\s*([0-9.]+)\s*,\s*out:\s*([0-9.]+)\s*\}\s*$') {
                $default = [ordered]@{ in = [double]$Matches[1]; out = [double]$Matches[2] }
                continue
            }
            if ($t -match '^models:\s*$') { $inModels = $true; $pending = ''; continue }
            # `default:` comes in two shapes: the inline flow form
            # `default: {in: 1.0, out: 3.0}` and the nested block form
            # configs\tiered-local.yaml actually uses, i.e. `default:` followed
            # by indented `in:`/`out:` lines. Both are handled; the nested form
            # is tracked through $pending exactly like the models entries.
            if ($t -match '^default:\s*$') { $pending = 'default'; $default = [ordered]@{ in = 0.0; out = 0.0 }; continue }
            if ($pending -eq 'default' -and $t -match '^in:\s*([0-9.]+)\s*$') { $default['in'] = [double]$Matches[1]; continue }
            if ($pending -eq 'default' -and $t -match '^out:\s*([0-9.]+)\s*$') { $default['out'] = [double]$Matches[1]; continue }
            if ($inModels) {
                if ($t -match '^([A-Za-z0-9._/-]+):\s*\{\s*in:\s*([0-9.]+)\s*,\s*out:\s*([0-9.]+)\s*\}\s*$') {
                    $models[$Matches[1]] = [ordered]@{ in = [double]$Matches[2]; out = [double]$Matches[3] }
                    $pending = ''
                    continue
                }
                if ($t -match '^([A-Za-z0-9._/-]+):\s*$') { $pending = $Matches[1]; continue }
                if ($pending -ne '' -and $t -match '^in:\s*([0-9.]+)\s*$') {
                    $models[$pending] = [ordered]@{ in = [double]$Matches[1]; out = 0.0 }
                    continue
                }
                if ($pending -ne '' -and $t -match '^out:\s*([0-9.]+)\s*$') {
                    if ($models.Contains($pending)) { $models[$pending]['out'] = [double]$Matches[1] }
                    $pending = ''
                    continue
                }
            }
        }
    }
    return [pscustomobject]@{ Default = $default; Models = $models }
}
