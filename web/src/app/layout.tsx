import type { Metadata } from "next";
import "./globals.css";
export const metadata: Metadata = {
  title: "Switchyard · Experiments and flags",
  description:
    "Inspect feature flags, run experiments and review rollout decisions.",
};
export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
