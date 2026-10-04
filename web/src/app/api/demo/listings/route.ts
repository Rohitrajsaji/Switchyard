import { randomUUID } from "node:crypto";
import { submitListing } from "@/lib/listings";
export async function POST(request: Request) {
  return submitListing(
    request,
    process.env.SWITCHYARD_ORIGIN ?? "http://localhost:3000",
    () => `listing_${randomUUID()}`,
  );
}
