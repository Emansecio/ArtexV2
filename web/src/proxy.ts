import { NextResponse } from "next/server";
import type { NextRequest } from "next/server";

const AUTH_PAGES = ["/welcome", "/login"];

export function proxy(request: NextRequest) {
  // Mock demo: no real login; allow every page (the client-side auth guard also allows through).
  if (process.env.NEXT_PUBLIC_MOCK === "1") return NextResponse.next();

  const { pathname } = request.nextUrl;
  const token = request.cookies.get("artex_token")?.value;
  const isAuthPage = AUTH_PAGES.some((p) => pathname === p || pathname.startsWith(`${p}/`));

  // Not logged in → redirect to the welcome page
  if (!token && !isAuthPage) {
    return NextResponse.redirect(new URL("/welcome", request.url));
  }

  // Logged in while visiting the welcome/login pages → redirect to the main UI
  if (token && isAuthPage) {
    return NextResponse.redirect(new URL("/function/tasks", request.url));
  }

  return NextResponse.next();
}

export const config = {
  // Skip Next.js internal routes, API routes, locale dictionaries, favicon, and static files under public/
  matcher: ["/((?!_next/static|_next/image|favicon\\.ico|api/|locales/|.*\\.(?:png|jpg|jpeg|gif|webp|svg|ico|woff2?|ttf|otf|json)$).*)"],
};
