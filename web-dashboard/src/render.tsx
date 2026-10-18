import React, { useEffect, useRef, useState, type ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { NativeSelect } from "@/components/ui/native-select";
import { Badge } from "@/components/ui/badge";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs";
import { Copy, LoaderCircle } from "lucide-react";
import { attributes, textOf, type ViewNode } from "./adapter";
import { useRuntime, WorkspaceLink } from "./runtime";
const renderChildren = (nodes: ViewNode[]) =>
  nodes.map((node, i) => (
    <RenderNode key={node.attrs.id || `${node.tag}-${i}`} node={node} />
  ));
function Form({ node }: { node: ViewNode }) {
  const { submit, pending, markDirty } = useRuntime();
  const form = useRef<HTMLFormElement>(null);
  const baseline = useRef<string | null>(null);
  const [dirty, setDirty] = useState(false);
  const fingerprint = (el: HTMLFormElement) =>
    JSON.stringify(
      [...new FormData(el)].map(([key, value]) => [
        key,
        typeof value === "string"
          ? value
          : [value.name, value.size, value.lastModified],
      ]),
    );
  useEffect(() => {
    const el = form.current;
    return () => {
      if (el) markDirty(el, false);
    };
  }, [markDirty]);
  return (
    <form
      {...attributes(node)}
      ref={(el) => {
        form.current = el;
        if (el && baseline.current === null) baseline.current = fingerprint(el);
      }}
      aria-busy={pending}
      data-dirty={dirty || undefined}
      onInput={(event) => {
        const changed = fingerprint(event.currentTarget) !== baseline.current;
        setDirty(changed);
        markDirty(event.currentTarget, changed);
      }}
      onSubmit={(event) => {
        event.preventDefault();
        submit(
          event.currentTarget,
          (event.nativeEvent as SubmitEvent)
            .submitter as HTMLButtonElement | null,
        );
      }}
    >
      {renderChildren(node.children)}
      {dirty && node.attrs.class?.includes("setting-form") && (
        <small className="unsaved-note">Unsaved change</small>
      )}
    </form>
  );
}
function PolicyTable({ node }: { node: ViewNode }) {
  const runtime = useRuntime();
  const groups = [
    ...new Set(
      node.children
        .flatMap((section) => section.children)
        .map((row) => row.attrs["data-setting-group"])
        .filter(Boolean),
    ),
  ];
  const initial = (runtime.page.focusSetting || location.hash).includes(
    "setting-",
  )
    ? node.children
        .flatMap((s) => s.children)
        .find(
          (r) =>
            r.attrs.id ===
            (runtime.page.focusSetting || location.hash.slice(1)),
        )?.attrs["data-setting-group"]
    : undefined;
  const [group, setGroup] = useState(initial || groups[0]);
  return (
    <div className="policy-browser">
      <Tabs value={group} onValueChange={setGroup}>
        <TabsList aria-label="Policy categories" className="policy-tabs">
          {groups.map((value) => (
            <TabsTrigger value={value} key={value}>
              {value}
            </TabsTrigger>
          ))}
        </TabsList>
        {groups.map((value) => (
          <TabsContent
            key={value}
            value={value}
            forceMount
            className={value === group ? "" : "hidden"}
          >
            <Table {...attributes(node)} className="settings policy-table">
              <TableHeader>
                {node.children
                  .filter((n) => n.tag === "thead")
                  .flatMap((n) => renderChildren(n.children))}
              </TableHeader>
              <TableBody>
                {node.children
                  .filter((n) => n.tag === "tbody")
                  .flatMap((n) =>
                    renderChildren(
                      n.children.filter(
                        (row) =>
                          !row.attrs["data-setting-group"] ||
                          row.attrs["data-setting-group"] === value,
                      ),
                    ),
                  )}
              </TableBody>
            </Table>
          </TabsContent>
        ))}
      </Tabs>
    </div>
  );
}
export function RenderNode({ node: n }: { node: ViewNode }): ReactNode {
  const runtime = useRuntime();
  const props = attributes(n);
  const cls = n.attrs.class || "";
  const children = () => renderChildren(n.children);
  if (n.tag === "#text") return n.text;
  if (n.tag === "form") return <Form node={n} />;
  if (n.tag === "a")
    return (
      <WorkspaceLink {...props} href={n.attrs.href}>
        {children()}
      </WorkspaceLink>
    );
  if (n.tag === "input") {
    const type = n.attrs.type || "text";
    if (type === "checkbox" || type === "radio")
      return (
        <Input
          {...props}
          type={type}
          defaultChecked={"checked" in n.attrs}
          defaultValue={n.attrs.value || "on"}
        />
      );
    const input = (
      <Input
        {...props}
        defaultValue={type === "file" ? undefined : n.attrs.value}
      />
    );
    return n.attrs["data-date-label"] ? (
      <label className="filter-date-field">
        <span>{n.attrs["data-date-label"]}</span>
        {input}
      </label>
    ) : (
      input
    );
  }
  if (n.tag === "textarea")
    return <Textarea {...props} defaultValue={textOf(n)} />;
  if (n.tag === "select") {
    const selected = n.children
      .filter((n) => n.tag === "option" && "selected" in n.attrs)
      .map((n) => n.attrs.value ?? textOf(n));
    return (
      <NativeSelect
        {...props}
        defaultValue={
          n.attrs.multiple !== undefined
            ? selected
            : (selected[0] ??
              n.children.find((n) => n.tag === "option")?.attrs.value)
        }
      >
        {children()}
      </NativeSelect>
    );
  }
  if (n.tag === "option")
    return (
      <option {...props} value={n.attrs.value}>
        {children()}
      </option>
    );
  if (n.tag === "button")
    return (
      <Button
        {...props}
        value={n.attrs.value}
        disabled={runtime.pending || "disabled" in n.attrs}
        variant={
          cls.includes("linkbtn")
            ? "ghost"
            : cls.includes("btn")
              ? "default"
              : "outline"
        }
        onClick={
          n.attrs["data-copy"]
            ? () => {
                const value = n.attrs["data-copy"];
                runtime.copy(
                  (document.getElementById(value) as HTMLInputElement | null)
                    ?.value || value,
                );
              }
            : undefined
        }
      >
        {n.attrs["data-copy"] && <Copy size={14} />} {children()}
      </Button>
    );
  if (n.tag === "span" && /\b(status|mode|flag|badge)\b/.test(cls))
    return (
      <Badge {...props} variant="secondary">
        {children()}
      </Badge>
    );
  if (n.tag === "table")
    return cls.includes("settings") ? (
      <PolicyTable node={n} />
    ) : (
      <Table {...props} className="operational-table">
        {children()}
      </Table>
    );
  if (n.tag === "thead")
    return <TableHeader {...props}>{children()}</TableHeader>;
  if (n.tag === "tbody") return <TableBody {...props}>{children()}</TableBody>;
  if (n.tag === "tr") return <TableRow {...props}>{children()}</TableRow>;
  if (n.tag === "th") return <TableHead {...props}>{children()}</TableHead>;
  if (n.tag === "td") return <TableCell {...props}>{children()}</TableCell>;
  if (n.tag === "details")
    return (
      <details
        {...props}
        open={
          n.children.some((n) => n.attrs.class?.includes("route-result")) ||
          "open" in n.attrs ||
          undefined
        }
      >
        {children()}
      </details>
    );
  if (["script", "style", "iframe", "object", "embed"].includes(n.tag))
    return null;
  if (n.tag === "img") return <img {...props} alt={n.attrs.alt || ""} />;
  if (n.tag === "br" || n.tag === "hr")
    return React.createElement(n.tag, props);
  return React.createElement(n.tag, props, children());
}
export function PageContent({ nodes }: { nodes: ViewNode[] }) {
  return <>{renderChildren(nodes)}</>;
}
