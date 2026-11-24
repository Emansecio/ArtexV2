import { intlLocale } from "@/i18n";

const dateTimeFmt = new Intl.DateTimeFormat(intlLocale, {
  dateStyle: "short",
  timeStyle: "medium",
});
const dateFmt = new Intl.DateTimeFormat(intlLocale, { dateStyle: "short" });
const timeFmt = new Intl.DateTimeFormat(intlLocale, { timeStyle: "medium" });
const numberFmt = new Intl.NumberFormat(intlLocale);

export function formatDateTime(v: Date | number | string): string {
  const d = v instanceof Date ? v : new Date(v);
  return Number.isNaN(d.getTime()) ? "" : dateTimeFmt.format(d);
}

export function formatDate(v: Date | number | string): string {
  const d = v instanceof Date ? v : new Date(v);
  return Number.isNaN(d.getTime()) ? "" : dateFmt.format(d);
}

export function formatTime(v: Date | number | string): string {
  const d = v instanceof Date ? v : new Date(v);
  return Number.isNaN(d.getTime()) ? "" : timeFmt.format(d);
}

export function formatNumber(n: number): string {
  return numberFmt.format(n);
}
