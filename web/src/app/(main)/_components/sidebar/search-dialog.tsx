"use client";

import * as React from "react";

import { useRouter } from "next/navigation";

import { Brain, MessageSquare, Plus, Search, ShieldAlert, Target } from "lucide-react";

import { StatusBadge } from "@/components/status-badge";
import { Button } from "@/components/ui/button";
import {
  Command,
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
  CommandSeparator,
} from "@/components/ui/command";
import { t } from "@/i18n";
import { api } from "@/lib/api";
import type { Task } from "@/lib/types";
import type { NavMainItem } from "@/navigation/sidebar/sidebar-items";
import { sidebarItems } from "@/navigation/sidebar/sidebar-items";

type SearchItem = {
  id: string;
  group: string;
  label: string;
  url: string;
  icon?: NavMainItem["icon"];
  disabled?: boolean;
  newTab?: boolean;
};

const sidebarGroupLabels = new Set(sidebarItems.flatMap((group) => (group.label ? [group.label] : [])));

function getSubItemGroup(groupLabel: string | undefined, itemTitle: string) {
  return sidebarGroupLabels.has(itemTitle) ? (groupLabel ?? t("Other")) : itemTitle;
}

const searchItems: SearchItem[] = sidebarItems.flatMap((group) =>
  group.items.flatMap((item) => {
    if (item.subItems) {
      return item.subItems.map((sub) => ({
        id: sub.id,
        group: getSubItemGroup(group.label, item.title),
        label: sub.title,
        url: sub.url,
        icon: sub.icon ?? item.icon,
        disabled: sub.disabled,
        newTab: sub.newTab,
      }));
    }
    return [
      {
        id: item.id,
        group: group.label ?? t("Other"),
        label: item.title,
        url: item.url,
        icon: item.icon,
        disabled: item.disabled,
        newTab: item.newTab,
      },
    ];
  }),
);

function getAvailableItems(items: SearchItem[]) {
  return items.filter((item) => !item.disabled && !item.url.includes("coming-soon"));
}

const recommendations = getAvailableItems(searchItems);

function groupBy(items: SearchItem[]) {
  const groups = [...new Set(items.map((item) => item.group))];
  return groups.map((group) => ({
    group,
    items: items.filter((item) => item.group === group),
  }));
}

export function SearchDialog({ showTrigger = true }: { showTrigger?: boolean }) {
  const [open, setOpen] = React.useState(false);
  const [query, setQuery] = React.useState("");
  const [recentTasks, setRecentTasks] = React.useState<Task[]>([]);
  const lastTasksFetchRef = React.useRef(0);
  const router = useRouter();

  React.useEffect(() => {
    const down = (e: KeyboardEvent) => {
      if ((e.key === "k" || e.key === "j") && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        setOpen((prev) => !prev);
      }
    };
    document.addEventListener("keydown", down);
    return () => document.removeEventListener("keydown", down);
  }, []);

  React.useEffect(() => {
    if (!open) return;
    if (Date.now() - lastTasksFetchRef.current < 15_000) return;
    let alive = true;
    api
      .tasks()
      .then((res) => {
        if (!alive) return;
        lastTasksFetchRef.current = Date.now();
        const sorted = [...res.tasks]
          .sort((a, b) => (b.last_activity_unix ?? b.created_unix ?? 0) - (a.last_activity_unix ?? a.created_unix ?? 0))
          .slice(0, 5);
        setRecentTasks(sorted);
      })
      .catch(() => {
        /* ignore transient errors */
      });
    return () => {
      alive = false;
    };
  }, [open]);

  const handleOpenChange = (value: boolean) => {
    setOpen(value);
    if (!value) setQuery("");
  };

  const navigate = (url: string, newTab?: boolean) => {
    handleOpenChange(false);
    if (newTab) {
      window.open(url, "_blank", "noopener,noreferrer");
    } else {
      router.push(url);
    }
  };

  const handleSelect = (item: SearchItem) => {
    if (item.disabled) return;
    navigate(item.url, item.newTab);
  };

  const renderGroups = (items: SearchItem[]) =>
    groupBy(items).map(({ group, items: groupItems }, index) => (
      <React.Fragment key={group}>
        {index > 0 && <CommandSeparator />}
        <CommandGroup heading={group}>
          {groupItems.map((item) => (
            <CommandItem
              disabled={item.disabled}
              key={`${group}-${item.id}`}
              value={`${item.group} ${item.label}`}
              onSelect={() => handleSelect(item)}
            >
              <span className="flex min-w-0 items-center gap-2">
                {item.icon && <item.icon />}
                <span className="truncate">{item.label}</span>
              </span>
            </CommandItem>
          ))}
        </CommandGroup>
      </React.Fragment>
    ));

  return (
    <>
      {showTrigger && (
        <Button
          onClick={() => handleOpenChange(true)}
          variant="outline"
          size="sm"
          className="h-7 w-40 justify-between gap-2 px-2.5 font-normal text-muted-foreground sm:w-52"
        >
          <span className="flex min-w-0 items-center gap-1.5">
            <Search className="size-3.5 shrink-0" />
            <span className="truncate text-xs">{t("Search")}</span>
          </span>
          <kbd className="inline-flex h-4.5 select-none items-center gap-0.5 rounded border bg-muted px-1 font-medium text-[10px]">
            <span className="text-[11px]">⌘</span>K
          </kbd>
        </Button>
      )}
      <CommandDialog open={open} onOpenChange={handleOpenChange}>
        <Command>
          <CommandInput placeholder={t("Search pages, tasks, and actions…")} value={query} onValueChange={setQuery} />
          <CommandList>
            <CommandEmpty>{t("No results found.")}</CommandEmpty>
            <CommandGroup heading={t("Quick actions")}>
              <CommandItem value={`quick ${t("New task")}`} onSelect={() => navigate("/function/tasks?new=1")}>
                <Plus />
                <span>{t("New task")}</span>
              </CommandItem>
              <CommandItem value={`quick ${t("Models & Fallbacks")}`} onSelect={() => navigate("/models")}>
                <Brain />
                <span>{t("Models & Fallbacks")}</span>
              </CommandItem>
              <CommandItem value={`quick ${t("Chat")}`} onSelect={() => navigate("/chat")}>
                <MessageSquare />
                <span>{t("Chat")}</span>
              </CommandItem>
              <CommandItem
                value={`quick ${t("Pending approvals")}`}
                onSelect={() => navigate("/system/intercept/approvals")}
              >
                <ShieldAlert />
                <span>{t("Pending approvals")}</span>
              </CommandItem>
            </CommandGroup>
            {recentTasks.length > 0 && (
              <>
                <CommandSeparator />
                <CommandGroup heading={t("Recent tasks")}>
                  {recentTasks.map((task) => (
                    <CommandItem
                      key={`task-${task.id}`}
                      value={`task ${task.id} ${task.name ?? ""} ${task.description}`}
                      onSelect={() => navigate(`/function/tasks/detail?id=${encodeURIComponent(task.id)}`)}
                    >
                      <Target className="shrink-0" />
                      <span className="min-w-0 flex-1 truncate">{task.name || task.description}</span>
                      <span className="font-mono text-[10px] text-muted-foreground">#{task.id}</span>
                      <StatusBadge domain="task" value={task.status} className="px-1.5 py-0 text-[10px]" />
                    </CommandItem>
                  ))}
                </CommandGroup>
              </>
            )}
            <CommandSeparator />
            {query ? renderGroups(searchItems) : renderGroups(recommendations)}
          </CommandList>
        </Command>
      </CommandDialog>
    </>
  );
}
