type Props = {
  newer?: () => void;
  older?: () => void;
  labels: { newer: string; older: string };
};

export function KeysetPager({ newer, older, labels }: Props) {
  if (!newer && !older) return null;
  return (
    <p className="actions">
      {newer && (
        <button type="button" className="secondary" onClick={newer}>
          {labels.newer}
        </button>
      )}
      {older && (
        <button type="button" className="secondary" onClick={older}>
          {labels.older}
        </button>
      )}
    </p>
  );
}
