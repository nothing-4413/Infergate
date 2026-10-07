# Readers for values the sample configs own.
#
# A gate renders its own copy of a sample config (substituting only the ports
# that would otherwise collide with a stale process) and hands that text to the
# gateway under test. When the gate then asserts that the RUNNING process
# reports one of those values, the expected value has to come out of the same
# text the gateway was given: a literal in the script is a third copy, and a
# deliberate retune of the sample -- a new threshold, a different store, a
# shorter context window -- turns a gateway that did exactly what it was
# configured to do red.
#
# Kept in one file because a reader that exists twice drifts once:
# scripts\verify-m1.ps1 and scripts\verify-m6.ps1 each carried their own copy of
# Get-ConfigScalar.

function Get-ConfigScalar {
    param([string]$Text, [string]$Block, [string]$Key)
    # A top-level block is named at column zero and owns everything indented
    # under it, up to the next column-zero line. Scoping the lookup that way is
    # what lets `ttl` come from the replay store rather than from whichever
    # `ttl` happens to appear first in the file, and `strategy` from `routing`
    # rather than from a nested strategy.
    $b = [regex]::Match($Text, "(?ms)^$([regex]::Escape($Block)):[^\r\n]*\r?\n(.*?)(?=^\S|\z)")
    if (-not $b.Success) { return $null }
    $k = [regex]::Match($b.Groups[1].Value, "(?m)^\s*$([regex]::Escape($Key)):\s*([^\s#]+)")
    if (-not $k.Success) { return $null }
    return $k.Groups[1].Value.Trim('"')
}

function Get-ConfigListScalar {
    param([string]$Text, [string]$Block, [string]$Key)
    # The entries of a list under a block are written "- key: value". Scoping the
    # lookup to the block is what keeps `name` from coming out of `models` or
    # `tenants`; scoping it to a list entry (the leading dash) is what keeps it
    # from coming out of one of the entry's own nested lists. The value returned
    # is the FIRST entry's, which is the one a gate compares an index like
    # upstreams[0] against.
    $b = [regex]::Match($Text, "(?ms)^$([regex]::Escape($Block)):[^\r\n]*\r?\n(.*?)(?=^\S|\z)")
    if (-not $b.Success) { return $null }
    $k = [regex]::Match($b.Groups[1].Value, "(?m)^\s*-\s*$([regex]::Escape($Key)):\s*([^\s#]+)")
    if (-not $k.Success) { return $null }
    return $k.Groups[1].Value.Trim('"')
}
