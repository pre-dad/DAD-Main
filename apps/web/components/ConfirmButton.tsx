"use client";
import { useEffect, useId, useRef, useState } from "react";

type Props = {
  /** Visible trigger text. */
  children: React.ReactNode;
  /** Text that names the target for assistive technology, appended to every button. */
  target?: React.ReactNode;
  /** The question asked before the action runs. */
  prompt?: React.ReactNode;
  confirmLabel: React.ReactNode;
  keepLabel?: React.ReactNode;
  onConfirm: () => void;
  disabled?: boolean;
  className?: string;
  /** Class for the confirm and keep buttons; defaults to the trigger's. */
  confirmClassName?: string;
  buttonRef?: (button: HTMLButtonElement | null) => void | (() => void);
};

/**
 * A destructive action that asks in the page instead of a native dialog: the trigger swaps for
 * Confirm and Keep buttons, focus moves to Confirm, and Keep returns it to the trigger.
 */
export function ConfirmButton({
  children,
  target,
  prompt,
  confirmLabel,
  keepLabel = "Keep",
  onConfirm,
  disabled,
  className = "secondary",
  confirmClassName,
  buttonRef,
}: Props) {
  const [asking, setAsking] = useState(false);
  const trigger = useRef<HTMLButtonElement | null>(null);
  const confirm = useRef<HTMLButtonElement | null>(null);
  const restore = useRef(false);
  const promptId = useId();
  const hidden = target ? (
    <>
      {" "}
      <span className="visuallyHidden">{target}</span>
    </>
  ) : null;

  useEffect(() => {
    if (asking) confirm.current?.focus();
    else if (restore.current) {
      restore.current = false;
      trigger.current?.focus();
    }
  }, [asking]);

  if (!asking)
    return (
      <button
        type="button"
        className={className}
        disabled={disabled}
        ref={(button) => {
          trigger.current = button;
          return buttonRef?.(button);
        }}
        onClick={() => setAsking(true)}
      >
        {children}
        {hidden}
      </button>
    );
  const style = confirmClassName ?? className;
  return (
    <>
      {prompt && (
        <span className="note" id={promptId}>
          {prompt}
        </span>
      )}
      <button
        type="button"
        className={style}
        disabled={disabled}
        ref={confirm}
        aria-describedby={prompt ? promptId : undefined}
        onClick={() => {
          setAsking(false);
          onConfirm();
        }}
      >
        {confirmLabel}
        {hidden}
      </button>
      <button
        type="button"
        className="secondary"
        disabled={disabled}
        onClick={() => {
          restore.current = true;
          setAsking(false);
        }}
      >
        {keepLabel}
        {hidden}
      </button>
    </>
  );
}
