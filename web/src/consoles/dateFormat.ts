import { createContext, useContext } from "react";

// The date format of the model on screen (DemoContext.date_format; "" leaves
// it to the reader's browser), for every widget that shows or takes a date.
export const DateFormatContext = createContext<string>("");

export function useDateFormat(): string {
  return useContext(DateFormatContext);
}
