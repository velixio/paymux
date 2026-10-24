import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { SHORTCUTS } from "./shortcuts";

export function ShortcutSheet({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="shortcut-sheet">
        <DialogHeader>
          <DialogTitle>Keyboard shortcuts</DialogTitle>
          <DialogDescription>
            Shortcuts are off while you type in a field.
          </DialogDescription>
        </DialogHeader>
        {SHORTCUTS.map((g) => (
          <section key={g.group}>
            <h3>{g.group}</h3>
            <dl>
              {g.items.map((item) => (
                <div key={item.label}>
                  <dt>{item.label}</dt>
                  <dd>
                    {item.combos.map((c, i) => (
                      <span className="shortcut-combo" key={i}>
                        {i > 0 && <span className="shortcut-or">or</span>}
                        {c.keys.map((k, j) => (
                          <span key={j}>
                            {j > 0 && c.then && (
                              <span className="shortcut-then">then</span>
                            )}
                            <kbd>{k}</kbd>
                          </span>
                        ))}
                      </span>
                    ))}
                  </dd>
                </div>
              ))}
            </dl>
          </section>
        ))}
      </DialogContent>
    </Dialog>
  );
}
