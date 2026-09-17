import { Search } from "lucide-react";
import type React from "react";
import { TextInput } from "./TextInput";

interface SearchInputProps extends React.InputHTMLAttributes<HTMLInputElement> {
  width?: number | string;
}

export function SearchInput({ width, style, ...rest }: SearchInputProps) {
  return (
    <div className="mvx-search" style={{ width, ...style }}>
      <Search size={14} className="mvx-search__icon" aria-hidden="true" />
      <TextInput type="search" {...rest} />
    </div>
  );
}
