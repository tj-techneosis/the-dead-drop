## Passing documents around

Write-ups, field notes, requests and automated reports go through the
**Dead Drop** (`deaddrop`), not copy and paste: load the **`deaddrop`**
skill first. "Dead drop this to Bob" is `deaddrop send --to bob --tag …`
with the document on stdin; "pull my mail about X" is
`deaddrop pull --tag x`, and once the work is done, file what was used with
`deaddrop mark read|archive ID…`. Never put a secret in a message.
