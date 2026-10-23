// Strips URL userinfo (username, password or both, including empty values)
// from free-form text, plus any trailing authority fragment left incomplete
// by a chunk boundary. Next logs complete proxy targets on connection
// errors, so captured child output must never publish credentials from
// API_INTERNAL_URL or any other configured URL.
export function redactUrlCredentials(text: string): string {
  const complete = text.replaceAll(
    /(\b[a-z][a-z0-9+.-]*:\/\/)([^/\s]*)@/gi,
    "$1[redacted]@",
  );
  // A credential prefix cut off before its closing at-sign would survive the
  // rule above, so an unterminated authority at end-of-text is redacted too.
  // The lookahead skips tokens that still contain an at-sign: rule one has
  // already rewritten their userinfo, so those tails stay diagnostic. A
  // complete URL without any at-sign loses its trailing host fragment; that
  // over-redaction is the safe direction for a diagnostic tail.
  return complete.replaceAll(
    /(\b[a-z][a-z0-9+.-]*:\/\/)(?![^\s]*@)[^\s]*$/gi,
    "$1[redacted]",
  );
}

// Builds the HTTP boundary failure message from captured child output,
// redacting credentials before the 16 KiB tail slice so an incomplete URL
// cannot survive truncation.
export function formatChildFailure(
  mode: "dev" | "start",
  stdout: string,
  stderr: string,
): string {
  return `Next ${mode} HTTP host boundary failed\n${redactUrlCredentials(stdout).slice(-16_384)}\n${redactUrlCredentials(stderr).slice(-16_384)}`;
}
