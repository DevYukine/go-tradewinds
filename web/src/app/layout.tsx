import type { Metadata, Viewport } from "next";
import localFont from "next/font/local";
import { Providers } from "./providers";
import "./globals.css";

const plex = localFont({
  src: [
    {
      path: "../../node_modules/@fontsource/ibm-plex-sans/files/ibm-plex-sans-latin-400-normal.woff2",
      weight: "400",
    },
    {
      path: "../../node_modules/@fontsource/ibm-plex-sans/files/ibm-plex-sans-latin-500-normal.woff2",
      weight: "500",
    },
    {
      path: "../../node_modules/@fontsource/ibm-plex-sans/files/ibm-plex-sans-latin-600-normal.woff2",
      weight: "600",
    },
  ],
  variable: "--font-plex",
  display: "swap",
});

const plexSerif = localFont({
  src: "../../node_modules/@fontsource/ibm-plex-serif/files/ibm-plex-serif-latin-600-normal.woff2",
  weight: "600",
  variable: "--font-plex-serif",
  display: "swap",
});

export const metadata: Metadata = {
  title: "Tradewinds | Fleet operations",
  description: "Recorded fleet, company finances and trading decisions.",
  robots: { index: false, follow: false },
};

export const viewport: Viewport = {
  width: "device-width",
  initialScale: 1,
  themeColor: "#1F6E7A",
};

export default function RootLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="en" className={`${plex.variable} ${plexSerif.variable}`}>
      <body>
        <a className="skip-link" href="#main-content">
          Skip to operations
        </a>
        <Providers>{children}</Providers>
      </body>
    </html>
  );
}
