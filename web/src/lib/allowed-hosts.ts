export function createAllowedHosts(
  extraHosts: string | undefined,
): ReadonlySet<string> {
  return new Set([
    "127.0.0.1:3400",
    "localhost:3400",
    "[::1]:3400",
    ...(extraHosts ?? "")
      .split(",")
      .map((host) => host.trim().toLowerCase())
      .filter(Boolean),
  ]);
}

export function isAllowedHost(
  host: string | null | undefined,
  allowed: ReadonlySet<string>,
): boolean {
  return Boolean(host && allowed.has(host.toLowerCase()));
}
