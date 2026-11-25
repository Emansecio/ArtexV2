"use client";

import { Bar, BarChart, CartesianGrid, XAxis, YAxis } from "recharts";

import { type ChartConfig, ChartContainer, ChartTooltip, ChartTooltipContent } from "@/components/ui/chart";

export type DailyTokenPoint = {
  date: string;
  input: number;
  output: number;
  cacheRead: number;
};

function fmtTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1000) return `${(n / 1000).toFixed(1)}k`;
  return String(n);
}

export function DailyTrendChart({
  data,
  config,
}: {
  data: DailyTokenPoint[];
  config: ChartConfig;
}) {
  return (
    <ChartContainer config={config} className="h-[200px] w-full">
      <BarChart data={data} margin={{ top: 4, right: 4, bottom: 0, left: 0 }} maxBarSize={40}>
        <CartesianGrid vertical={false} />
        <XAxis
          dataKey="date"
          tickLine={false}
          axisLine={false}
          tick={{ fontSize: 10 }}
          tickMargin={6}
          interval="preserveStartEnd"
        />
        <YAxis
          tickFormatter={(v) => fmtTokens(Number(v))}
          tickLine={false}
          axisLine={false}
          tick={{ fontSize: 10 }}
          width={44}
        />
        <ChartTooltip
          cursor={false}
          content={
            <ChartTooltipContent
              indicator="line"
              formatter={(value, name) => (
                <div className="flex w-full items-center justify-between gap-4">
                  <span>
                    {(config as Record<string, { label?: string }>)[String(name)]?.label ?? String(name)}
                  </span>
                  <span className="font-mono font-semibold tabular-nums">{fmtTokens(Number(value))}</span>
                </div>
              )}
            />
          }
        />
        <Bar dataKey="input" stackId="1" fill="var(--color-input)" radius={[0, 0, 0, 0]} />
        <Bar dataKey="output" stackId="1" fill="var(--color-output)" radius={[0, 0, 0, 0]} />
        <Bar dataKey="cacheRead" stackId="1" fill="var(--color-cacheRead)" radius={[4, 4, 0, 0]} />
      </BarChart>
    </ChartContainer>
  );
}
