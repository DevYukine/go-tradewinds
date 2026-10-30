import { resolve } from "node:path";
import { loadEnvFile } from "node:process";
import type { NextConfig } from "next";

// The package scripts run from web/. Share the Go service's root environment
// file without overriding variables already supplied by the shell or Next.
try {
  loadEnvFile(resolve(process.cwd(), "..", ".env"));
} catch (error) {
  if (!(error instanceof Error && "code" in error && error.code === "ENOENT")) {
    throw error;
  }
}

const backend = (
  process.env.API_INTERNAL_URL ?? "http://127.0.0.1:8585"
).replace(/\/$/, "");

const config: NextConfig = {
  reactStrictMode: true,
  poweredByHeader: false,
  images: {
    remotePatterns: [
      {
        protocol: "https",
        hostname: "raw.githubusercontent.com",
        pathname: "/PokeAPI/sprites/master/sprites/pokemon/**",
      },
    ],
  },
  async rewrites() {
    // src/proxy.ts validates the original Host before this rewrite replaces it.
    return [{ source: "/api/:path*", destination: `${backend}/api/:path*` }];
  },
};

export default config;
