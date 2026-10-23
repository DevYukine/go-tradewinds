import { NextResponse, type NextRequest } from "next/server";

import { createAllowedHosts, isAllowedHost } from "@/lib/allowed-hosts";

const allowedHosts = createAllowedHosts(process.env.DASHBOARD_ALLOWED_HOSTS);

export function middleware(request: NextRequest) {
  if (!isAllowedHost(request.headers.get("host"), allowedHosts)) {
    return new NextResponse("Forbidden host\n", {
      status: 403,
      headers: { "Content-Type": "text/plain; charset=utf-8" },
    });
  }
  return NextResponse.next();
}

export const config = {
  matcher: "/:path*",
  runtime: "nodejs",
};
