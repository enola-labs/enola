select order_date, count(*) as orders from raw_orders group by order_date
