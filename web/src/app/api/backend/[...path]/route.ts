import { proxy } from "@/lib/proxy";
export const runtime = "nodejs";
async function handle(
  request: Request,
  context: { params: Promise<{ path: string[] }> },
) {
  return proxy(request, (await context.params).path, {
    baseURL: process.env.SWITCHYARD_API_URL ?? "http://127.0.0.1:8080",
    origin: process.env.SWITCHYARD_ORIGIN ?? "http://localhost:3000",
  });
}
export { handle as GET, handle as POST, handle as PUT, handle as DELETE };
